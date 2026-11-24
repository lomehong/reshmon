//go:build windows

package app

import (
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

func appWithSlices(slices ...procSlice) *App {
	a := &App{Cfg: DefaultConfig()}
	a.procSlices = make([]procSlice, len(slices)+1)
	for i, s := range slices {
		a.procSlices[i] = s
	}
	a.sliceAt = len(slices) // 空一拍在末尾（未填满的环）
	return a
}

// 回溯最近的一份：请求时刻落在两份之间时取更近的那份；Agent 账本取自
// 总量缓冲里时间上最接近的一拍。
func TestAtNearest(t *testing.T) {
	base := time.Now()
	a := appWithSlices(
		procSlice{T: base.Add(-90 * time.Second).UnixMilli(), CPU: 10, Procs: []sliceProc{{Name: "a.exe"}}},
		procSlice{T: base.Add(-30 * time.Second).UnixMilli(), CPU: 80, Procs: []sliceProc{{Name: "b.exe", CpuPct: 80}}},
		procSlice{T: base.Add(-2 * time.Second).UnixMilli(), CPU: 5, Procs: []sliceProc{{Name: "c.exe"}}},
	)
	// 距 -30s 那份更近
	r := a.At(base.Add(-25 * time.Second))
	if r.Blind || r.Slice == nil || r.Slice.CPU != 80 || r.Slice.Procs[0].Name != "b.exe" {
		t.Fatalf("应回溯到最近的一份: %+v", r)
	}
	if r.DeltaMs < -10000 || r.DeltaMs > 10000 {
		t.Fatalf("DeltaMs 应反映实际偏差: %d", r.DeltaMs)
	}
	// 精确命中
	r = a.At(base.Add(-30 * time.Second))
	if r.Blind || r.Slice.CPU != 80 || r.DeltaMs != 0 {
		t.Fatalf("精确命中应零偏差: %+v", r)
	}
	if len(r.Agents) != 0 {
		t.Fatalf("没有总量缓冲时不应有 Agent 账本: %+v", r.Agents)
	}

	// Agent 账本：取总量缓冲里离该时刻最近的一拍（-30.5s 比 -29s 更近）
	a.ring = store.NewRing(8)
	a.ring.Push(model.Totals{T: base.Add(-30100 * time.Millisecond).UnixMilli(),
		Agents: []model.AgentStat{{Name: "ZCode", CpuPct: 30}}})
	a.ring.Push(model.Totals{T: base.Add(-29 * time.Second).UnixMilli(),
		Agents: []model.AgentStat{{Name: "Other", CpuPct: 1}}})
	r = a.At(base.Add(-30 * time.Second))
	if len(r.Agents) != 1 || r.Agents[0].Name != "ZCode" {
		t.Fatalf("Agent 账本应取最接近的一拍: %+v", r.Agents)
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
	if !r.Blind || r.Slice != nil {
		t.Fatalf("超出保留范围应判盲区: %+v", r)
	}
	if r.Reason == "" {
		t.Fatal("超出范围应说明原因")
	}
	r = a.At(base.Add(time.Minute))
	if !r.Blind {
		t.Fatal("未来时刻应判盲区")
	}
	// 覆盖范围不短于趋势图最长档：2 小时前（在最新一份之前一点）必须有据可回溯
	recent := appWithSlices(procSlice{T: base.Add(-2 * time.Hour).UnixMilli(), CPU: 10},
		procSlice{T: base.Add(-30 * time.Second).UnixMilli(), CPU: 20})
	if r := recent.At(base.Add(-119 * time.Minute)); r.Blind {
		t.Fatalf("2 小时档的尾部应可回溯（覆盖范围不短于趋势图）: %+v", r)
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

// 抽稀：默认间隔下每 3 拍存一份，首份立即存。
func TestPushProcSliceDecimates(t *testing.T) {
	a := &App{Cfg: DefaultConfig()} // 2s → 每 3 拍
	base := time.Now()
	for i := 0; i < 7; i++ {
		a.pushProcSlice(&model.Snapshot{
			TS:  base.Add(time.Duration(i) * 2 * time.Second),
			CPU: model.CPUStat{BusyPct: float64(i)},
		})
	}
	out := a.procSliceSnapshot()
	if len(out) != 3 {
		t.Fatalf("7 拍应存 3 份（第 0、3、6 拍），得到 %d", len(out))
	}
	for i, want := range []float64{0, 3, 6} {
		if out[i].CPU != want {
			t.Fatalf("第 %d 份应为第 %.0f 拍，得到 %.0f", i, want, out[i].CPU)
		}
	}
}

// 环形覆盖：槽位固定为 sliceCap，超出后保留最新的 sliceCap 份（时间升序）。
func TestProcRingWrap(t *testing.T) {
	a := &App{Cfg: Config{Interval: 2 * time.Second}}
	base := time.Now()
	n := 3 * (sliceCap + 3)
	for i := 0; i < n; i++ {
		a.pushProcSlice(&model.Snapshot{
			TS:  base.Add(time.Duration(i) * 2 * time.Second),
			CPU: model.CPUStat{BusyPct: float64(i)},
		})
	}
	out := a.procSliceSnapshot()
	if len(out) != sliceCap {
		t.Fatalf("环形应固定保留 %d 份，得到 %d", sliceCap, len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].T < out[i-1].T {
			t.Fatalf("概览未按时间升序: 第 %d 份", i)
		}
	}
	// 共存了 sliceCap+3 份（第 0、3、…、n-3 拍），挤掉最早的 3 份
	if first, last := out[0].CPU, out[len(out)-1].CPU; first != 9 || last != float64(n-3) {
		t.Fatalf("保留范围错误: 首份 %.0f（应为 9） 末份 %.0f（应为 %d）", first, last, n-3)
	}
}

// 抽稀步长：让固定槽位恰好覆盖 2 小时，时间粒度基本恒定。
func TestSliceEveryFor(t *testing.T) {
	cases := map[time.Duration]int{
		2 * time.Second:        3,  // 3600 拍 / 1200 槽
		500 * time.Millisecond: 12, // 14400 拍 / 1200 槽
		30 * time.Second:       1,  // 拍数少于槽位，不必抽稀
		0:                      3,  // 非法间隔回退默认
	}
	for interval, want := range cases {
		if got := sliceEveryFor(interval); got != want {
			t.Fatalf("sliceEveryFor(%v) = %d want %d", interval, got, want)
		}
	}
}

// 每份概览是四个维度的 Top 并集：CPU 不高但重 IO / 重 GPU / 重内存的进程
// 都要留——否则按"磁盘忙"回溯时看不到真正的 IO 大户。
func TestBuildProcSliceCoversAllDims(t *testing.T) {
	var procs []model.ProcStat
	for i := 0; i < 60; i++ { // 60 个 CPU 很高、其余维度为 0 的进程
		procs = append(procs, model.ProcStat{PID: uint32(1000 + i), Name: "cpu", CpuPct: float64(60 - i)})
	}
	procs = append(procs,
		model.ProcStat{PID: 1, Name: "iohog", ReadBps: 1e9},
		model.ProcStat{PID: 2, Name: "gpuhog", GpuPct: 99},
		model.ProcStat{PID: 3, Name: "memhog", MemPrivB: 1 << 32},
	)
	s := buildProcSlice(&model.Snapshot{TS: time.Now(), Procs: procs}, sliceKeep)
	has := map[uint32]bool{}
	for _, p := range s.Procs {
		has[p.PID] = true
	}
	for _, pid := range []uint32{1, 2, 3} {
		if !has[pid] {
			t.Fatalf("PID %d 是其所在维度的头名，必须保留", pid)
		}
	}
	if len(s.Procs) > sliceKeep {
		t.Fatalf("超出保留上限: %d", len(s.Procs))
	}
	for i := 1; i < len(s.Procs); i++ {
		if s.Procs[i-1].CpuPct < s.Procs[i].CpuPct {
			t.Fatal("默认应按 CPU 降序")
		}
	}
}
