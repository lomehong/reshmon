package store

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

// Writer 按天写 totals-YYYYMMDD.jsonl 与 procs-YYYYMMDD.jsonl；
// 跨天轮转时把前一天 gzip 压缩，并按保留天数清理旧文件。
// 当天文件不压缩（进程还可能续写），读取方两类文件都会合并。
// totals 与 procs 两路文件独立打开、独立降级：一路打不开不连坐另一路。
type Writer struct {
	dir       string
	procEvery int
	retention int
	day       string
	fT        *os.File
	fP        *os.File
	closed    bool
	nowFn     func() time.Time // 测试注入
}

func NewWriter(dir string, procEvery, retentionDays int) (*Writer, error) {
	return newWriterClock(dir, procEvery, retentionDays, time.Now)
}

// newWriterClock 可注入时钟的构造：repairDir/prune 在构造期内即依赖
// 「今天」，测试必须先定钟再触发——否则测试写死的日期一过真实时钟
// 就会把"今天的活跃文件"补压缩掉（时间炸弹）。
func newWriterClock(dir string, procEvery, retentionDays int, now func() time.Time) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if procEvery < 1 {
		procEvery = 1
	}
	w := &Writer{dir: dir, procEvery: procEvery, retention: retentionDays, nowFn: now}
	// 先修复历史（崩溃残留/漏压缩），再清理过期文件。
	w.repairDir(w.nowFn())
	w.prune()
	return w, nil
}

// SetNow 注入时钟（测试用：模拟跨天轮转）。
func (w *Writer) SetNow(fn func() time.Time) { w.nowFn = fn }

func dayKey(t time.Time) string { return t.Format("20060102") }

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func totalsPath(dir, day string) string { return filepath.Join(dir, "totals-"+day+".jsonl") }
func procsPath(dir, day string) string  { return filepath.Join(dir, "procs-"+day+".jsonl") }

// Append 写一条总量记录（必要时先轮转）。写失败即关句柄：
// 设备错误后同一句柄会持续失败，关掉让下次调用懒开新句柄自愈
// （否则要等午夜轮转才恢复，期间整天静默丢数据）。
func (w *Writer) Append(t model.Totals) error {
	if w.closed {
		return fmt.Errorf("writer 已关闭")
	}
	w.rotate(w.nowFn())
	if w.fT == nil {
		f, err := os.OpenFile(totalsPath(w.dir, w.day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		w.fT = f
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	n, err := w.fT.Write(append(b, '\n'))
	if err != nil || n != len(b)+1 {
		w.fT.Close()
		w.fT = nil
		if err == nil {
			err = fmt.Errorf("短写 %d/%d", n, len(b)+1)
		}
	}
	return err
}

type ProcsRec struct {
	T   int64            `json:"t"`
	Top []model.ProcStat `json:"top"`
}

// AppendProcs 写一条进程切片（调用方按 procEvery 节流）。写失败即关
// 句柄自愈（同 Append）。
func (w *Writer) AppendProcs(ts time.Time, procs []model.ProcStat) error {
	if w.closed {
		return fmt.Errorf("writer 已关闭")
	}
	w.rotate(w.nowFn())
	if w.fP == nil {
		f, err := os.OpenFile(procsPath(w.dir, w.day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		w.fP = f
	}
	b, err := json.Marshal(ProcsRec{T: ts.UnixMilli(), Top: procs})
	if err != nil {
		return err
	}
	n, err := w.fP.Write(append(b, '\n'))
	if err != nil || n != len(b)+1 {
		w.fP.Close()
		w.fP = nil
		if err == nil {
			err = fmt.Errorf("短写 %d/%d", n, len(b)+1)
		}
	}
	return err
}

// rotate 处理日期切换（关闭旧句柄、压缩前一天、清理过期文件）。
// 打开失败不在这里报错：Append/AppendProcs 各自懒打开并重试，
// 一路文件失败不阻断另一路。
func (w *Writer) rotate(now time.Time) {
	day := dayKey(now)
	if day == w.day {
		return
	}
	if w.fT != nil {
		w.fT.Close()
		w.fT = nil
	}
	if w.fP != nil {
		w.fP.Close()
		w.fP = nil
	}
	if w.day != "" {
		gzipFile(totalsPath(w.dir, w.day))
		gzipFile(procsPath(w.dir, w.day))
	}
	w.day = day
	w.prune()
}

func (w *Writer) Close() {
	w.closed = true
	if w.fT != nil {
		w.fT.Close()
		w.fT = nil
	}
	if w.fP != nil {
		w.fP.Close()
		w.fP = nil
	}
}

// gzipFile 压缩单文件：成功后 sync 再删原文件（断电窗口内宁可 raw 多留
// 一份，不让 gz 未落盘 raw 已删导致整天数据丢失）；任何失败保持 raw 不动。
func gzipFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	out, err := os.Create(path + ".gz")
	if err != nil {
		f.Close()
		return
	}
	gz := gzip.NewWriter(out)
	_, err = io.Copy(gz, f)
	f.Close()
	if err != nil || gz.Close() != nil {
		out.Close()
		os.Remove(path + ".gz")
		return
	}
	_ = out.Sync()
	out.Close()
	os.Remove(path)
}

// repairDir 启动修复与补压缩：
//   - 过去日期的 raw 没有对应 gz（进程没跨过午夜就退出了）→ 补压缩；
//   - raw 与 gz 并存（gzip 中途崩溃留下的截断 gz，或 Copy 完成、Remove
//     之前的崩溃）→ 以 raw 为权威：删 gz 重做；
//   - 当天的 raw+gz 并存 → 删 gz（当天文件活跃续写中，读取端会双读）。
func (w *Writer) repairDir(now time.Time) {
	today := dayKey(now)
	for _, prefix := range []string{"totals-", "procs-"} {
		entries, err := os.ReadDir(w.dir)
		if err != nil {
			return
		}
		haveRaw, haveGz := map[string]bool{}, map[string]bool{}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			day, ok := fileDay(name)
			if !ok {
				continue
			}
			if strings.HasSuffix(name, ".gz") {
				haveGz[day] = true
			} else {
				haveRaw[day] = true
			}
		}
		for day := range haveRaw {
			raw := filepath.Join(w.dir, prefix+day+".jsonl")
			if haveGz[day] {
				_ = os.Remove(raw + ".gz")
			}
			if day < today {
				gzipFile(raw)
			}
		}
	}
}

// MinRetentionDays 分钟级降采样层（totals-min-*）的保留天数。
// 原始层（2s 粒度）过 RetentionDays 后降采样为分钟级均值再保留一年——
// 长区间趋势分析（如内存/提交的数月爬坡）因此可用，体积约为原始的 1/30。
const MinRetentionDays = 365

// prune 删除超过保留天数的 totals-*/procs-* 文件（含 .gz）。
// totals 在删除前降采样为分钟级 min 文件（幂等：已存在则跳过；
// 写入为临时文件+原子改名，崩溃不会留下半截 min 文件）。
func (w *Writer) prune() {
	if w.retention <= 0 {
		return
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	cutoff := dayKey(w.nowFn().AddDate(0, 0, -w.retention))
	for _, e := range entries {
		day, ok := fileDay(e.Name())
		if !ok || day >= cutoff {
			continue
		}
		if strings.HasPrefix(e.Name(), "totals-") {
			if err := downsampleDay(w.dir, day); err != nil {
				// 降采样失败不删除原始文件，明天 prune 重试
				continue
			}
		}
		_ = os.Remove(filepath.Join(w.dir, e.Name()))
	}
}

// minDayOf 从 min 文件名解析日期：totals-min-YYYYMMDD.jsonl.gz。
func minDayOf(name string) (string, bool) {
	const prefix = "totals-min-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl.gz") {
		return "", false
	}
	day := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".jsonl.gz")
	if len(day) != 8 {
		return "", false
	}
	if _, err := strconv.Atoi(day); err != nil {
		return "", false
	}
	return day, true
}

// downsampleDay 把某天的 totals 降采样为分钟级均值，写入
// totals-min-<day>.jsonl.gz（已存在则幂等跳过）。procs 切片不降采样
// （体积大、长区间无意义），随原始文件一起到期删除。
func downsampleDay(dir, day string) error {
	target := filepath.Join(dir, "totals-min-"+day+".jsonl.gz")
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	dayStart, _ := time.ParseInLocation("20060102", day, time.Local)
	dayEnd := dayStart.Add(24 * time.Hour)
	recs, _, err := LoadTotals(dir, dayStart, dayEnd)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	// 按分钟桶求均值（数字字段），标签取首见；GPU 的 Key 必须透传
	// （report/趋势按 Key 分组，置空会全部塌缩成一条）
	type acc struct {
		n                 int
		cpu, memp, commit float64
		nr, ns            float64
		disks             map[string]*struct {
			id, l                 string
			busy, rb, wb, lat, fp float64
		}
		gpus map[string]*struct {
			key, name  string
			util, vram float64
		}
		agents map[string]*model.AgentStat
	}
	buckets := map[int64]*acc{}
	order := []int64{}
	for _, r := range recs {
		b := r.T / 60000 * 60000
		a := buckets[b]
		if a == nil {
			a = &acc{disks: map[string]*struct {
				id, l                 string
				busy, rb, wb, lat, fp float64
			}{}, gpus: map[string]*struct {
				key, name  string
				util, vram float64
			}{}, agents: map[string]*model.AgentStat{}}
			buckets[b] = a
			order = append(order, b)
		}
		a.n++
		a.cpu += r.CPU
		a.memp += r.MemUsedPct
		a.commit += r.CommitPct
		a.nr += r.NetRecvBps
		a.ns += r.NetSentBps
		for _, d := range r.Disks {
			da := a.disks[d.ID]
			if da == nil {
				da = &struct {
					id, l                 string
					busy, rb, wb, lat, fp float64
				}{id: d.ID, l: d.L}
				a.disks[d.ID] = da
			}
			da.busy += d.Busy
			da.rb += d.RBps
			da.wb += d.WBps
			da.lat += d.LatMs
			da.fp += d.FreePct
		}
		for _, g := range r.GPUs {
			ga := a.gpus[g.Key]
			if ga == nil {
				ga = &struct {
					key, name  string
					util, vram float64
				}{key: g.Key, name: g.Name}
				a.gpus[g.Key] = ga
			}
			ga.util += g.Util
			ga.vram += g.VramPct
		}
		for _, ag := range r.Agents {
			aa := a.agents[ag.Name]
			if aa == nil {
				c := ag
				aa = &c
				a.agents[ag.Name] = aa
				continue
			}
			aa.CpuPct += ag.CpuPct
			aa.MemPrivB += ag.MemPrivB
			aa.ReadBps += ag.ReadBps
			aa.WriteBps += ag.WriteBps
			aa.GpuPct += ag.GpuPct
			aa.VramB += ag.VramB
			aa.Procs += ag.Procs
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var buf bytes.Buffer
	for _, b := range order {
		a := buckets[b]
		n := float64(a.n)
		out := model.Totals{
			T: b, CPU: a.cpu / n, MemUsedPct: a.memp / n, CommitPct: a.commit / n,
			NetRecvBps: a.nr / n, NetSentBps: a.ns / n,
		}
		for _, da := range a.disks {
			out.Disks = append(out.Disks, model.DiskTotal{
				ID: da.id, L: da.l, Busy: da.busy / n, RBps: da.rb / n, WBps: da.wb / n,
				LatMs: da.lat / n, FreePct: da.fp / n,
			})
		}
		for _, ga := range a.gpus {
			out.GPUs = append(out.GPUs, model.GPUTotal{Key: ga.key, Name: ga.name, Util: ga.util / n, VramPct: ga.vram / n})
		}
		for _, aa := range a.agents {
			c := *aa
			c.CpuPct /= n
			c.ReadBps /= n
			c.WriteBps /= n
			c.GpuPct /= n
			c.Procs = int(float64(c.Procs)/n + 0.5)
			out.Agents = append(out.Agents, c)
		}
		line, merr := json.Marshal(out)
		if merr != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	// 压缩到临时文件后原子改名：崩溃不留半截 min 文件
	tmp := target + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err = zw.Write(buf.Bytes()); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, target)
}

// fileDay 从文件名解析日期（totals-20260102.jsonl / .jsonl.gz 均可）。
func fileDay(name string) (string, bool) {
	for _, prefix := range []string{"totals-", "procs-"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// 先剥 .gz 再剥 .jsonl——顺序反了会连不带 .gz 的也解析失败
		// （历史 bug：.gz 归档文件因此永远不会被 retention 清理）。
		rest := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".gz"), ".jsonl")
		if len(rest) == 8 {
			if _, err := strconv.Atoi(rest); err == nil {
				return rest, true
			}
		}
	}
	return "", false
}

// readJSONL 逐行读 JSONL；.gz 后缀自动解压；缺失文件返回空。
// 坏行跳过；流级错误（如 gzip 截断）向上返回，由调用方决定降级方式。
func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip 头损坏: %w", err)
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4*1024*1024), 16*1024*1024) // 进程切片行较大
	var out []T
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err == nil {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

// LoadTotals 读取 [from,to] 的总量记录（.jsonl 与 .jsonl.gz 合并，按时间升序）。
// 逐条按时间窗过滤；raw 与 gz 并存时以 raw 为准；单文件损坏（如 gzip
// 截断）跳过并记入 warnings，不让整天查询失败。已过期降采样的日子，
// 原始层缺失时自动读分钟级 totals-min-*（同一日原始层仍存在时跳过
// min 层防重复）。
func LoadTotals(dir string, from, to time.Time) ([]model.Totals, []string, error) {
	var out []model.Totals
	var warns []string
	fm, tm := from.UnixMilli(), to.UnixMilli()
	appendFiltered := func(recs []model.Totals) {
		for _, r := range recs {
			if r.T >= fm && r.T <= tm {
				out = append(out, r)
			}
		}
	}
	covered := map[string]bool{} // 已有原始数据的日期（min 层跳过）
	for d := truncateDay(from); !d.After(truncateDay(to)); d = d.AddDate(0, 0, 1) {
		day := dayKey(d)
		recs, err := readJSONL[model.Totals](totalsPath(dir, day))
		if err != nil {
			warns = append(warns, fmt.Sprintf("totals-%s 读取失败（已跳过）: %v", day, err))
			recs = nil
		}
		if len(recs) > 0 {
			covered[day] = true
			appendFiltered(recs)
			continue
		}
		gzRecs, err := readJSONL[model.Totals](totalsPath(dir, day) + ".gz")
		if err != nil {
			warns = append(warns, fmt.Sprintf("totals-%s.gz 读取失败（已跳过）: %v", day, err))
			continue
		}
		if len(gzRecs) > 0 {
			covered[day] = true
			appendFiltered(gzRecs)
			continue
		}
	}
	// 分钟级降采样层：日期入窗且该日无原始数据
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		day, ok := minDayOf(e.Name())
		if !ok || covered[day] {
			continue
		}
		d, perr := time.ParseInLocation("20060102", day, time.Local)
		if perr != nil || d.Before(truncateDay(from)) || d.After(truncateDay(to)) {
			continue
		}
		minRecs, err := readJSONL[model.Totals](filepath.Join(dir, e.Name()))
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s 读取失败（已跳过）: %v", e.Name(), err))
			continue
		}
		covered[day] = true
		appendFiltered(minRecs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, warns, nil
}

// LoadProcs 读取 [from,to] 的进程切片记录（降级策略与 LoadTotals 一致）。
func LoadProcs(dir string, from, to time.Time) ([]ProcsRec, []string, error) {
	var out []ProcsRec
	var warns []string
	fm, tm := from.UnixMilli(), to.UnixMilli()
	appendFiltered := func(recs []ProcsRec) {
		for _, r := range recs {
			if r.T >= fm && r.T <= tm {
				out = append(out, r)
			}
		}
	}
	for d := truncateDay(from); !d.After(truncateDay(to)); d = d.AddDate(0, 0, 1) {
		day := dayKey(d)
		recs, err := readJSONL[ProcsRec](procsPath(dir, day))
		if err != nil {
			warns = append(warns, fmt.Sprintf("procs-%s 读取失败（已跳过）: %v", day, err))
			recs = nil
		}
		if len(recs) > 0 {
			appendFiltered(recs)
			continue
		}
		gzRecs, err := readJSONL[ProcsRec](procsPath(dir, day) + ".gz")
		if err != nil {
			warns = append(warns, fmt.Sprintf("procs-%s.gz 读取失败（已跳过）: %v", day, err))
			continue
		}
		appendFiltered(gzRecs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, warns, nil
}

// TrimProcs 取 CPU/内存/IO/GPU 四个维度 Top 的并集（按 PID 去重），
// 上限 n 条——落盘与 API 输出用，保住"每个维度最凶的嫌疑人"。
func TrimProcs(procs []model.ProcStat, n int) []model.ProcStat {
	if n <= 0 || len(procs) <= n {
		return procs
	}
	seen := map[uint32]bool{}
	var out []model.ProcStat
	take := func(k int, less func(a, b model.ProcStat) bool) {
		idx := make([]model.ProcStat, len(procs))
		copy(idx, procs)
		sort.Slice(idx, func(i, j int) bool { return less(idx[i], idx[j]) })
		for _, p := range idx {
			if k <= 0 || len(out) >= n {
				return
			}
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			out = append(out, p)
			k--
		}
	}
	per := n/4 + 1
	take(per, func(a, b model.ProcStat) bool { return a.CpuPct > b.CpuPct })
	take(per, func(a, b model.ProcStat) bool { return a.MemPrivB > b.MemPrivB })
	take(per, func(a, b model.ProcStat) bool { return a.ReadBps+a.WriteBps > b.ReadBps+b.WriteBps })
	take(per, func(a, b model.ProcStat) bool { return a.GpuPct > b.GpuPct })
	if len(out) < n {
		take(n, func(a, b model.ProcStat) bool { return a.CpuPct > b.CpuPct })
	}
	return out
}
