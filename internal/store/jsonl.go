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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if procEvery < 1 {
		procEvery = 1
	}
	w := &Writer{dir: dir, procEvery: procEvery, retention: retentionDays, nowFn: time.Now}
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

// Append 写一条总量记录（必要时先轮转）。
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
	_, err = w.fT.Write(append(b, '\n'))
	return err
}

type ProcsRec struct {
	T   int64            `json:"t"`
	Top []model.ProcStat `json:"top"`
}

// AppendProcs 写一条进程切片（调用方按 procEvery 节流）。
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
	_, err = w.fP.Write(append(b, '\n'))
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

// prune 删除超过保留天数的 totals-*/procs-* 文件（含 .gz）。
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
		if day, ok := fileDay(e.Name()); ok && day < cutoff {
			_ = os.Remove(filepath.Join(w.dir, e.Name()))
		}
	}
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
// 截断）跳过并记入 warnings，不让整天查询失败。
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
	for d := truncateDay(from); !d.After(truncateDay(to)); d = d.AddDate(0, 0, 1) {
		day := dayKey(d)
		recs, err := readJSONL[model.Totals](totalsPath(dir, day))
		if err != nil {
			warns = append(warns, fmt.Sprintf("totals-%s 读取失败（已跳过）: %v", day, err))
			recs = nil
		}
		if len(recs) > 0 {
			appendFiltered(recs)
			continue
		}
		gzRecs, err := readJSONL[model.Totals](totalsPath(dir, day) + ".gz")
		if err != nil {
			warns = append(warns, fmt.Sprintf("totals-%s.gz 读取失败（已跳过）: %v", day, err))
			continue
		}
		appendFiltered(gzRecs)
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
