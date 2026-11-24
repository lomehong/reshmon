//go:build windows

package app

import (
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

func mkSlice(offsetSec int, cpu float64) procSlice {
	return procSlice{
		T:   time.Now().Add(time.Duration(offsetSec) * time.Second).UnixMilli(),
		CPU: cpu,
		Procs: []sliceProc{
			{Name: "busy.exe", PID: 100, CpuPct: cpu},
			{Name: "idle.exe", PID: 200, CpuPct: 0.1},
		},
	}
}

func appWithSlices(slices ...procSlice) *App {
	a := &App{Cfg: DefaultConfig()}
	a.procSlices = make([]procSlice, len(slices)+1)
	for i, s := range slices {
		a.procSlices[i] = s
	}
	a.sliceAt = len(slices) // 空一拍在末尾（未填满的环）
	return a
}

// 回溯最近的一拍：请求时刻落在两拍之间时取更近的那一拍。
func TestAtNearest(t *testing.T) {
	base := time.Now()
	a := appWithSlices(
		procSlice{T: base.Add(-90 * time.Second).UnixMilli(), CPU: 10, Procs: []sliceProc{{Name: "a.exe"}}},
		procSlice{T: base.Add(-30 * time.Second).UnixMilli(), CPU: 80, Procs: []sliceProc{{Name: "b.exe", CpuPct: 80}}},
		procSlice{T: base.Add(-2 * time.Second).UnixMilli(), CPU: 5, Procs: []sliceProc{{Name: "c.exe"}}},
	)
	// 距 -30s 那拍更近
	r := a.At(base.Add(-25 * time.Second))
	if r.Blind || r.Slice == nil || r.Slice.CPU != 80 || r.Slice.Procs[0].Name != "b.exe" {
		t.Fatalf("应回溯到最近的一拍: %+v", r)
	}
	if r.DeltaMs < -10000 || r.DeltaMs > 10000 {
		t.Fatalf("DeltaMs 应反映实际偏差: %d", r.DeltaMs)
	}
	// 精确命中
	r = a.At(base.Add(-2 * time.Second))
	if r.Blind || r.Slice.CPU != 5 || r.DeltaMs != 0 {
		t.Fatalf("精确命中应零偏差: %+v", r)
	}
}

// 关键语义：请求时刻落入数据缺失区间（冻结/睡眠）时必须标记 blind，
// 不能返回一个空列表让人误读成"当时没有进程"。
func TestAtBlindInGap(t *testing.T) {
	base := time.Now()
	a := appWithSlices(
		procSlice{T: base.Add(-20 * time.Minute).UnixMilli(), CPU: 30, Procs: []sliceProc{{Name: "before.exe"}}},
		// 中间缺了 19 分钟（记录器冻结）
		procSlice{T: base.Add(-90 * time.Second).UnixMilli(), CPU: 20, Procs: []sliceProc{{Name: "after.exe"}}},
	)
	r := a.At(base.Add(-10 * time.Minute)) // 缺口正中
	if !r.Blind {
		t.Fatalf("缺口内应判为盲区，实际返回切片: %+v", r.Slice)
	}
	if r.Reason == "" {
		t.Fatal("盲区必须给出原因说明")
	}
	if r.Slice != nil {
		t.Fatal("盲区不应返回切片数据（避免被误读为当时的真实状态）")
	}
}

// 超出内存保留范围：同样如实标记盲区并提示改用 report。
func TestAtOutOfRange(t *testing.T) {
	base := time.Now()
	a := appWithSlices(procSlice{T: base.Add(-time.Minute).UnixMilli(), CPU: 10})
	r := a.At(base.Add(-3 * time.Hour))
	if !r.Blind {
		t.Fatal("超出保留范围应判盲区")
	}
	if r.Slice != nil {
		t.Fatal("超出范围不应返回切片")
	}
	r = a.At(base.Add(time.Minute))
	if !r.Blind {
		t.Fatal("未来时刻应判盲区")
	}
}

// 空缓冲（服务刚启动）不 panic、如实返回盲区。
func TestAtEmpty(t *testing.T) {
	a := &App{Cfg: DefaultConfig()}
	r := a.At(time.Now())
	if !r.Blind || r.Slice != nil {
		t.Fatalf("空缓冲应返回盲区: %+v", r)
	}
}

// 环形缓冲覆盖与按间隔换算的保留拍数。
func TestProcRingWrap(t *testing.T) {
	a := &App{Cfg: DefaultConfig(), procSlices: make([]procSlice, 3), sliceAt: 0}
	for i := 0; i < 5; i++ {
		a.pushProcSlice(&model.Snapshot{
			TS:    time.Now().Add(time.Duration(i) * time.Second),
			CPU:   model.CPUStat{BusyPct: float64(i)},
			Procs: []model.ProcStat{{Name: "p.exe", PID: uint32(i + 1)}},
		})
	}
	out := a.procSliceSnapshot()
	if len(out) != 3 {
		t.Fatalf("环形应只保留 3 拍，得到 %d", len(out))
	}
	// 时间升序；最新的 CPU 应为 4（第 5 拍）
	for i := 1; i < len(out); i++ {
		if out[i].T < out[i-1].T {
			t.Fatalf("切片未按时间升序: %+v", out)
		}
	}
	if out[len(out)-1].CPU != 4 {
		t.Fatalf("最新拍应为第 5 拍: %+v", out[len(out)-1])
	}
}

func TestRingProcsFor(t *testing.T) {
	// 目标覆盖约 1 小时，随间隔换算
	if n := ringProcsFor(2 * time.Second); n != 1800 {
		t.Fatalf("2s 间隔应保留 1800 拍，得到 %d", n)
	}
	if n := ringProcsFor(500 * time.Millisecond); n != 7200 {
		t.Fatalf("500ms 间隔应保留 7200 拍，得到 %d", n)
	}
	if n := ringProcsFor(0); n != ringProcsBase {
		t.Fatalf("零间隔应回退默认值，得到 %d", n)
	}
	// 下限保护
	if n := ringProcsFor(time.Hour); n != 60 {
		t.Fatalf("超长间隔应有下限 60，得到 %d", n)
	}
}

// 每拍只留 Top N，避免内存随进程数膨胀。
func TestBuildProcSliceTopN(t *testing.T) {
	var procs []model.ProcStat
	for i := 0; i < 100; i++ {
		procs = append(procs, model.ProcStat{Name: "p", PID: uint32(i), CpuPct: float64(i)})
	}
	s := buildProcSlice(&model.Snapshot{TS: time.Now(), Procs: procs}, 10)
	if len(s.Procs) != 10 {
		t.Fatalf("应只保留 Top 10，得到 %d", len(s.Procs))
	}
	if s.Procs[0].CpuPct != 99 {
		t.Fatalf("Top 应按 CPU 降序: %+v", s.Procs[0])
	}
}
