package store

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

func TestRing(t *testing.T) {
	r := NewRing(5)
	for i := 0; i < 8; i++ {
		r.Push(model.Totals{T: int64(i)})
	}
	if r.Len() != 5 {
		t.Fatalf("容量截断失败: %d", r.Len())
	}
	last := r.Last(3)
	if len(last) != 3 || last[0].T != 5 || last[2].T != 7 {
		t.Fatalf("Last 顺序错误: %+v", last)
	}
}

func TestWriterRotateAndLoad(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 1, 14)
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.Local)
	w.SetNow(func() time.Time { return day1 })
	if err := w.Append(model.Totals{T: day1.Add(1 * time.Second).UnixMilli(), CPU: 10}); err != nil {
		t.Fatal(err)
	}
	if err := w.AppendProcs(day1.Add(2*time.Second), []model.ProcStat{{PID: 1, Name: "a.exe", CpuPct: 5}}); err != nil {
		t.Fatal(err)
	}
	// 跨天触发轮转：前一天文件被 gzip
	day2 := time.Date(2026, 1, 3, 8, 0, 0, 0, time.Local)
	w.SetNow(func() time.Time { return day2 })
	if err := w.Append(model.Totals{T: day2.UnixMilli(), CPU: 20}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	totals, _, err := LoadTotals(dir, day1, day2)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 || totals[0].CPU != 10 || totals[1].CPU != 20 {
		t.Fatalf("跨天读取（含 gz）失败: %+v", totals)
	}
	procs, _, err := LoadProcs(dir, day1, day2)
	if err != nil || len(procs) != 1 || procs[0].Top[0].Name != "a.exe" {
		t.Fatalf("进程切片读取失败: %+v %v", procs, err)
	}
}

func TestTrimProcs(t *testing.T) {
	var procs []model.ProcStat
	for i := 0; i < 300; i++ {
		procs = append(procs, model.ProcStat{PID: uint32(i), CpuPct: float64(i % 50), MemPrivB: uint64(300-i) * 1e6, GpuPct: float64(i % 7)})
	}
	out := TrimProcs(procs, 40)
	if len(out) > 40 {
		t.Fatalf("超限: %d", len(out))
	}
	seen := map[uint32]bool{}
	for _, p := range out {
		if seen[p.PID] {
			t.Fatalf("重复 PID: %d", p.PID)
		}
		seen[p.PID] = true
	}
	// CPU 最高的必须保留
	hasTop := false
	for _, p := range out {
		if p.CpuPct == 49 {
			hasTop = true
		}
	}
	if !hasTop {
		t.Fatal("CPU Top 未保留")
	}
}

func TestIncidentLog(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenIncidentLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.Append(Incident{T: now.Add(-time.Minute).UnixMilli(), Dim: "cpu", From: "ok", To: "warn"})
	l.Append(Incident{T: now.UnixMilli(), Dim: "cpu", From: "warn", To: "sat"})
	l.Close()
	incs, err := LoadIncidents(dir, now.Add(-time.Hour), now)
	if err != nil || len(incs) != 2 {
		t.Fatalf("事件读取失败: %+v %v", incs, err)
	}
	if incs[0].To != "warn" || incs[1].To != "sat" {
		t.Fatal("事件顺序错误")
	}
}

// 回归：fileDay 曾因 TrimSuffix 顺序（先 .jsonl 后 .gz）导致 .gz 归档
// 永远不被 retention 识别清理——磁盘按天无限增长。
func TestFileDayGz(t *testing.T) {
	cases := []struct {
		name string
		day  string
		ok   bool
	}{
		{"totals-20260102.jsonl", "20260102", true},
		{"totals-20260102.jsonl.gz", "20260102", true},
		{"procs-20260102.jsonl", "20260102", true},
		{"procs-20260102.jsonl.gz", "20260102", true},
		{"totals-20260102.jsonl.gz.tmp", "", false},
		{"incidents.jsonl", "", false},
		{"totals-notaday.jsonl", "", false},
	}
	for _, c := range cases {
		day, ok := fileDay(c.name)
		if ok != c.ok || (ok && day != c.day) {
			t.Fatalf("fileDay(%q) = %q,%v want %q,%v", c.name, day, ok, c.day, c.ok)
		}
	}
}

func TestPruneRetension(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 1, 14)
	if err != nil {
		t.Fatal(err)
	}
	w.Close() // 只要 prune 逻辑，不写数据
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)
	old := "20261001"    // 25 天前：raw 和 gz 都必须删
	recent := "20261025" // 昨天：必须保留
	for _, day := range []string{old, recent} {
		raw := totalsPath(dir, day)
		if err := os.WriteFile(raw, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gz, err := os.Create(raw + ".gz")
		if err != nil {
			t.Fatal(err)
		}
		gz.Close()
	}
	w.SetNow(func() time.Time { return now })
	w.prune()
	for _, name := range []string{"totals-" + old + ".jsonl", "totals-" + old + ".jsonl.gz"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("超期文件未被清理: %s", name)
		}
	}
	for _, name := range []string{"totals-" + recent + ".jsonl", "totals-" + recent + ".jsonl.gz"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("保留期内文件被误删: %s (%v)", name, err)
		}
	}
}

// 回归：进程当天启动、未跨午夜退出 → 前几天的 raw 永不 gzip（磁盘占用
// 放大 ~10 倍）。NewWriter 启动时应补压缩过去的 raw。
func TestStartupBackfillGzip(t *testing.T) {
	dir := t.TempDir()
	yesterday := time.Date(2026, 10, 25, 23, 0, 0, 0, time.Local)
	today := yesterday.Add(24 * time.Hour)
	day := dayKey(yesterday)
	ts := yesterday.Add(10 * time.Minute).UnixMilli()
	for _, prefix := range []string{"totals-", "procs-"} {
		if err := os.WriteFile(filepath.Join(dir, prefix+day+".jsonl"),
			[]byte(fmt.Sprintf(`{"t":%d,"cpu":5}`+"\n", ts)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// today 的 raw 不应被压缩（活跃文件）
	if err := os.WriteFile(totalsPath(dir, dayKey(today)),
		[]byte(fmt.Sprintf(`{"t":%d,"cpu":1}`+"\n", today.Add(time.Minute).UnixMilli())), 0o644); err != nil {
		t.Fatal(err)
	}

	// 直接调用 repairDir 语义（NewWriter 内部同样路径）
	w, err := NewWriter(dir, 1, 14)
	if err != nil {
		t.Fatal(err)
	}
	w.SetNow(func() time.Time { return today })
	w.repairDir(today)
	w.Close()

	if _, err := os.Stat(totalsPath(dir, day) + ".gz"); err != nil {
		t.Fatalf("昨日 raw 未被补压缩: %v", err)
	}
	if _, err := os.Stat(procsPath(dir, day) + ".gz"); err != nil {
		t.Fatalf("昨日 procs raw 未被补压缩: %v", err)
	}
	if _, err := os.Stat(totalsPath(dir, day)); !os.IsNotExist(err) {
		t.Fatal("补压缩后 raw 应被删除")
	}
	if _, err := os.Stat(totalsPath(dir, dayKey(today))); err != nil {
		t.Fatal("今日 raw 不应被压缩/删除")
	}
	if _, err := os.Stat(totalsPath(dir, dayKey(today)) + ".gz"); !os.IsNotExist(err) {
		t.Fatal("今日不应产生 gz")
	}
	// 补压缩后的数据仍可读
	recs, _, err := LoadTotals(dir, yesterday, yesterday.Add(24*time.Hour))
	if err != nil || len(recs) != 1 || recs[0].CPU != 5 {
		t.Fatalf("补压缩后读取失败: %+v %v", recs, err)
	}
}

// 回归：gzip 中途崩溃残留的半截 .gz 与完整 raw 并存时——读取端以 raw
// 为准（不被 gz 截断拖死），单文件损坏降级为 warning 而非整体失败。
func TestLoadDegradesCorruptGz(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.Local)
	day := dayKey(base)
	t1 := base.Add(1 * time.Minute).UnixMilli()
	t2 := base.Add(2 * time.Minute).UnixMilli()
	// 完整 raw + 半截 gz
	if err := os.WriteFile(totalsPath(dir, day),
		[]byte(fmt.Sprintf(`{"t":%d,"cpu":10}`+"\n"+`{"t":%d,"cpu":20}`+"\n", t1, t2)), 0o644); err != nil {
		t.Fatal(err)
	}
	gz := gzipBytes([]byte(fmt.Sprintf(`{"t":%d,"cpu":10}`+"\n", t1)))
	if err := os.WriteFile(totalsPath(dir, day)+".gz", gz[:len(gz)/2], 0o644); err != nil { // 截断
		t.Fatal(err)
	}
	recs, _, err := LoadTotals(dir, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("raw 为权威应读出 2 条: %+v", recs)
	}

	// 只有损坏 gz（raw 已删）：跳过并告警，不整体失败
	dir2 := t.TempDir()
	if err := os.WriteFile(totalsPath(dir2, day)+".gz", gz[:len(gz)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	recs2, warns, err := LoadTotals(dir2, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 0 {
		t.Fatalf("损坏 gz 应按空处理: %+v", recs2)
	}
	if len(warns) == 0 {
		t.Fatal("损坏 gz 应产生 warning")
	}
}

func TestLoadTimeFilter(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.Local)
	w, err := NewWriter(dir, 1, 14)
	if err != nil {
		t.Fatal(err)
	}
	w.SetNow(func() time.Time { return base })
	for i := 0; i < 10; i++ {
		if err := w.Append(model.Totals{T: base.Add(time.Duration(i) * time.Minute).UnixMilli(), CPU: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	from := base.Add(2 * time.Minute)
	to := base.Add(5 * time.Minute)
	recs, _, err := LoadTotals(dir, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || recs[0].CPU != 2 || recs[3].CPU != 5 {
		t.Fatalf("时间窗过滤失败: %+v", recs)
	}
}

// 回归：totals 打不开不应连坐阻断 procs 写入——两路文件独立懒打开，
// 各自失败各自报错、各自重试。
func TestAppendAfterCloseErrors(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 1, 14)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(model.Totals{T: 1}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := w.Append(model.Totals{T: 2}); err == nil {
		t.Fatal("关闭后 Append 应报错")
	}
	if err := w.AppendProcs(time.Now(), nil); err == nil {
		t.Fatal("关闭后 AppendProcs 应报错")
	}
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}
