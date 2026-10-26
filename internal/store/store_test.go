package store

import (
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

	totals, err := LoadTotals(dir, day1, day2)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 || totals[0].CPU != 10 || totals[1].CPU != 20 {
		t.Fatalf("跨天读取（含 gz）失败: %+v", totals)
	}
	procs, err := LoadProcs(dir, day1, day2)
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
