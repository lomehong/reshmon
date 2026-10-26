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
type Writer struct {
	dir       string
	procEvery int
	retention int
	day       string
	fT        *os.File
	fP        *os.File
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
	if err := w.rotate(w.nowFn()); err != nil {
		return err
	}
	if w.fT == nil {
		return fmt.Errorf("writer 已关闭")
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
	if err := w.rotate(w.nowFn()); err != nil {
		return err
	}
	if w.fP == nil {
		return fmt.Errorf("writer 已关闭")
	}
	b, err := json.Marshal(ProcsRec{T: ts.UnixMilli(), Top: procs})
	if err != nil {
		return err
	}
	_, err = w.fP.Write(append(b, '\n'))
	return err
}

func (w *Writer) rotate(now time.Time) error {
	day := dayKey(now)
	if day == w.day && w.fT != nil {
		return nil
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
	var err error
	w.fT, err = os.OpenFile(totalsPath(w.dir, day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.fP, err = os.OpenFile(procsPath(w.dir, day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.day = day
	w.prune()
	return nil
}

func (w *Writer) Close() {
	if w.fT != nil {
		w.fT.Close()
		w.fT = nil
	}
	if w.fP != nil {
		w.fP.Close()
		w.fP = nil
	}
}

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
	out.Close()
	os.Remove(path)
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

func fileDay(name string) (string, bool) {
	for _, prefix := range []string{"totals-", "procs-"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".jsonl"), ".gz")
		if len(rest) == 8 {
			if _, err := strconv.Atoi(rest); err == nil {
				return rest, true
			}
		}
	}
	return "", false
}

// readJSONL 逐行读 JSONL；.gz 后缀自动解压；缺失文件返回空。
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
			return nil, nil // 损坏文件按空处理
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
func LoadTotals(dir string, from, to time.Time) ([]model.Totals, error) {
	var out []model.Totals
	for d := truncateDay(from); !d.After(truncateDay(to)); d = d.AddDate(0, 0, 1) {
		day := dayKey(d)
		for _, p := range []string{totalsPath(dir, day), totalsPath(dir, day) + ".gz"} {
			recs, err := readJSONL[model.Totals](p)
			if err != nil {
				return nil, err
			}
			out = append(out, recs...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}

// LoadProcs 读取 [from,to] 的进程切片记录。
func LoadProcs(dir string, from, to time.Time) ([]ProcsRec, error) {
	var out []ProcsRec
	for d := truncateDay(from); !d.After(truncateDay(to)); d = d.AddDate(0, 0, 1) {
		day := dayKey(d)
		for _, p := range []string{procsPath(dir, day), procsPath(dir, day) + ".gz"} {
			recs, err := readJSONL[ProcsRec](p)
			if err != nil {
				return nil, err
			}
			out = append(out, recs...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
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
