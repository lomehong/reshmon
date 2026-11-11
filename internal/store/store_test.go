package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
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

// 回归（v0.5.0 实测数据丢失）：句柄级写故障必须自愈——磁盘冻结期间
// 写在坏句柄上会持续失败，Append 必须关句柄重开重试，失败计数可见。
func TestIncidentLogSelfHeal(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenIncidentLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.Append(Incident{T: now.Add(-2 * time.Minute).UnixMilli(), Dim: "cpu", From: "ok", To: "warn"})
	// 模拟句柄级故障：底层句柄直接关掉（设备错误后的等价状态）
	l.mu.Lock()
	l.f.Close()
	l.f = nil
	l.mu.Unlock()

	l.Append(Incident{T: now.Add(-time.Minute).UnixMilli(), Dim: "cpu", From: "warn", To: "sat"})
	if n, msg := l.Failures(); n == 0 {
		t.Fatal("故障期间失败计数应非零")
	} else if msg == "" {
		t.Fatal("失败原因应记录")
	}
	l.Append(Incident{T: now.UnixMilli(), Dim: "cpu", From: "sat", To: "ok"}) // 自愈重开
	if n, _ := l.Failures(); n != 0 {
		t.Fatalf("自愈后失败计数应清零: %d", n)
	}
	l.Close()

	incs, err := LoadIncidents(dir, now.Add(-time.Hour), now)
	if err != nil || len(incs) != 3 {
		t.Fatalf("自愈后事件应完整落盘: %+v %v", incs, err)
	}
}

// 尾部修复：冻结中断留下的残行（无换行结尾）被隔离，不影响后续追加与读取。
func TestIncidentLogTailRepair(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "incidents.jsonl"),
		[]byte(`{"t":1,"dim":"cpu","from":"ok","to":"warn","detai`), 0o644); err != nil { // 残行，无 \n
		t.Fatal(err)
	}
	l, err := OpenIncidentLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.Append(Incident{T: now.UnixMilli(), Dim: "mem", From: "ok", To: "warn"})
	l.Close()
	incs, err := LoadIncidents(dir, time.Unix(0, 0), now)
	if err != nil || len(incs) != 1 || incs[0].Dim != "mem" {
		t.Fatalf("残行应被隔离、新事件应可读: %+v %v", incs, err)
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

	// 用注入时钟构造（NewWriter 内部同样路径）：修复/清理在构造期即以
	// 注入的"今天"执行——与真实日期解耦，不再是时间炸弹。
	w, err := newWriterClock(dir, 1, 14, func() time.Time { return today })
	if err != nil {
		t.Fatal(err)
	}
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

// 回归：过期日降采样为分钟级 min 文件（幂等、原子改名、混读去重）。
func TestDownsampleOnPrune(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local)
	w, err := newWriterClock(dir, 1, 14, func() time.Time { return day })
	if err != nil {
		t.Fatal(err)
	}
	// 3 个分钟桶、每桶 2 条（10/20 → 15；30/40 → 35；50/60 → 55）
	vals := []float64{10, 20, 30, 40, 50, 60}
	for i, v := range vals {
		ts := day.Add(time.Duration(i/2) * time.Minute).Add(time.Duration(i%2) * 30 * time.Second)
		if err := w.Append(model.Totals{T: ts.UnixMilli(), CPU: v, MemUsedPct: 50}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.AppendProcs(day.Add(time.Minute), []model.ProcStat{{PID: 1, Name: "x.exe"}}); err != nil {
		t.Fatal(err)
	}
	// 跨过保留期：触发轮转 + prune → 降采样 + 删除
	later := day.AddDate(0, 0, 15)
	w.SetNow(func() time.Time { return later })
	if err := w.Append(model.Totals{T: later.UnixMilli(), CPU: 5}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	minPath := filepath.Join(dir, "totals-min-20260601.jsonl.gz")
	if _, err := os.Stat(minPath); err != nil {
		t.Fatalf("min 文件未生成: %v", err)
	}
	if _, err := os.Stat(totalsPath(dir, "20260601")); !os.IsNotExist(err) {
		t.Fatal("过期 raw 应被删除")
	}
	// 幂等：再跑一次 prune 不重复写入
	w2, err := newWriterClock(dir, 1, 14, func() time.Time { return later })
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()

	// 混读：原始层缺失 → 自动读 min 层，分钟均值正确
	recs, _, err := LoadTotals(dir, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].CPU != 15 || recs[1].CPU != 35 || recs[2].CPU != 55 {
		t.Fatalf("分钟均值错误: %+v", recs)
	}
	if recs[0].T != day.UnixMilli() { // day 本身即 10:00 整分，首桶对齐
		t.Fatalf("桶时刻应取整分钟: %d want %d", recs[0].T, day.UnixMilli())
	}
	// 重复读取不过量（min 幂等 + 无重复）
	recs2, _, _ := LoadTotals(dir, day, day.Add(24*time.Hour))
	if len(recs2) != 3 {
		t.Fatalf("重复读取数量不稳定: %d", len(recs2))
	}
	// procs 切片不降采样：过期即删
	if _, err := os.Stat(procsPath(dir, "20260601")); !os.IsNotExist(err) {
		t.Fatal("过期 procs 应被删除")
	}
}

// 崩溃窗口去重：min 已写、raw 未删时，LoadTotals 只取 raw 不重复。
func TestLoadTotalsMinDedupe(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local)
	// raw 在（t 用当日内真实时刻，别拍脑袋的 epoch）
	if err := os.WriteFile(totalsPath(dir, "20260601"),
		[]byte(fmt.Sprintf(`{"t":%d,"cpu":10}`+"\n", day.Add(time.Hour).UnixMilli())), 0o644); err != nil {
		t.Fatal(err)
	}
	// min 也在（崩溃窗口）
	recs := []model.Totals{{T: day.Add(time.Hour).UnixMilli(), CPU: 99}}
	if err := writeMinFile(dir, "20260601", recs); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadTotals(dir, day, day.Add(24*time.Hour))
	if err != nil || len(got) != 1 || got[0].CPU != 10 {
		t.Fatalf("raw 存在时不得读 min 层: %+v %v", got, err)
	}
	// 删 raw → 读 min
	os.Remove(totalsPath(dir, "20260601"))
	got, _, err = LoadTotals(dir, day, day.Add(24*time.Hour))
	if err != nil || len(got) != 1 || got[0].CPU != 99 {
		t.Fatalf("raw 缺失时应读 min 层: %+v %v", got, err)
	}
}

func writeMinFile(dir, day string, recs []model.Totals) error {
	var buf bytes.Buffer
	for _, r := range recs {
		b, _ := json.Marshal(r)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	f, err := os.Create(filepath.Join(dir, "totals-min-"+day+".jsonl.gz"))
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	_, werr := zw.Write(buf.Bytes())
	if cerr := zw.Close(); werr == nil {
		werr = cerr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}
