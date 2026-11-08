//go:build windows

package app

import (
	"sync"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

// 真实采样 + 并发读写冒烟（-race 下跑）：HTTP 侧读 Current/Incidents/Errs
// 与采样循环并发——历史上 Errors/incTail 未受锁保护的数据竞争在此路径上。
func TestAppConcurrentSmoke(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Interval = 250 * time.Millisecond
	cfg.Record = false // 不落盘：聚焦并发路径
	cfg.derive()

	a, err := Start(cfg)
	if err != nil {
		t.Skipf("采样器不可用（无 PDH 权限的环境）: %v", err)
	}
	defer a.Close()

	deadline := time.Now().Add(1500 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				snap, rep := a.Current()
				if snap != nil {
					_ = snap.CPU.BusyPct
					_ = rep.Overall
					_ = a.Errs()
					_ = a.Incidents()
					_ = a.Ring().Last(8)
				}
				time.Sleep(30 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	snap, _ := a.Current()
	if snap == nil {
		t.Fatal("1.5s 后应已有快照")
	}
	if snap.CPU.Cores <= 0 {
		t.Fatalf("CPU 核数应大于 0: %+v", snap.CPU)
	}
}

// GPU 总量身份键 = LUID（稳定），显示名单独携带。
func TestTotalsFromGPUKeyStable(t *testing.T) {
	s := &model.Snapshot{
		TS: time.Now(),
		GPUs: []model.GPUStat{
			{LUIDKey: "0x1_0x2", Name: "NVIDIA RTX", UtilCapPct: 50},
		},
		NICs: []model.NICStat{
			{Name: "Intel(R) Ethernet", RecvBps: 100, SentBps: 50},
			{Name: "Hyper-V Virtual Ethernet Adapter", RecvBps: 100, SentBps: 50},
			{Name: "vEthernet (WSL)", RecvBps: 30, SentBps: 10},
		},
	}
	tt := TotalsFrom(s)
	if len(tt.GPUs) != 1 || tt.GPUs[0].Key != "0x1_0x2" || tt.GPUs[0].Name != "NVIDIA RTX" {
		t.Fatalf("GPU 身份键应为 LUID: %+v", tt.GPUs)
	}
	// 虚拟网卡不计入总量（vNIC/pNIC 双计会翻倍）
	if tt.NetRecvBps != 100 || tt.NetSentBps != 50 {
		t.Fatalf("网络总量应剔除虚拟网卡: %v/%v", tt.NetRecvBps, tt.NetSentBps)
	}
}

// -no-record 模式下事件时间线仍然工作（incLog 为 nil 不再整体早退）；
// 事件按新语义需驻留 30s 确认。
func TestNoRecordIncidentsLive(t *testing.T) {
	a := &App{prevStatus: map[string]string{}, pending: map[string]pendingInc{}}
	okRep := health.Report{Overall: model.StatusOK, Dims: []health.DimStatus{{Key: "cpu", Label: "CPU", Status: model.StatusOK}}}
	warnRep := health.Report{Overall: model.StatusWarn, Dims: []health.DimStatus{{Key: "cpu", Label: "CPU", Status: model.StatusWarn, Detail: "均值 70.0%"}}}
	base := time.Now()
	a.recordTransitions(okRep, &model.Snapshot{TS: base})                         // 建基线：无事件
	a.recordTransitions(warnRep, &model.Snapshot{TS: base.Add(time.Second)})      // ok→warn：挂起
	a.recordTransitions(warnRep, &model.Snapshot{TS: base.Add(31 * time.Second)}) // 驻留达标：确认
	incs := a.Incidents()
	var cpuInc int
	for _, in := range incs {
		if in.Dim == "cpu" {
			cpuInc++
			if in.From != model.StatusOK || in.To != model.StatusWarn {
				t.Fatalf("迁移记录错误: %+v", in)
			}
		}
	}
	if cpuInc != 1 {
		t.Fatalf("no-record 模式确认后事件应入时间线: %+v", incs)
	}
}

// 事件防抖：驻留不足 30s 的往返脉冲不落事件；达标确认；时间戳为迁移
// 首次观察时刻；跳档立即确认原迁移。
func TestIncidentDwellDebounce(t *testing.T) {
	a := &App{prevStatus: map[string]string{}, pending: map[string]pendingInc{}}
	base := time.Now()
	rep := func(st string) health.Report {
		return health.Report{Overall: st, Dims: []health.DimStatus{{Key: "cpu", Label: "CPU", Status: st, Detail: "d"}}}
	}
	tick := func(offset time.Duration, st string) {
		a.recordTransitions(rep(st), &model.Snapshot{TS: base.Add(offset)})
	}
	// 脉冲：ok→warn→ok，全程 <30s → 无事件，状态回 ok
	tick(0, model.StatusOK)
	tick(2*time.Second, model.StatusWarn)
	tick(4*time.Second, model.StatusOK)
	if n := len(a.Incidents()); n != 0 {
		t.Fatalf("秒级脉冲不应产生事件: %d", n)
	}
	if a.prevStatus["cpu"] != model.StatusOK || len(a.pending) != 0 {
		t.Fatalf("脉冲后状态错误: prev=%v pending=%v", a.prevStatus, a.pending)
	}
	// 驻留达标：ok→warn 后停留 → 30s 后确认，时间戳 = 迁移发生时刻。
	// StatusMap 同时含 overall 与 cpu 两个维度，事件成对——只按 cpu 断言。
	t0 := 10 * time.Second
	tick(t0, model.StatusWarn)
	if n := len(a.Incidents()); n != 0 {
		t.Fatal("未达驻留时长不应提前落事件")
	}
	tick(t0+minDwellMs*time.Millisecond+2*time.Second, model.StatusWarn)
	cpuIncs := cpuDim(a.Incidents())
	if len(cpuIncs) != 1 {
		t.Fatalf("驻留达标应确认 1 条 cpu 事件: %+v", a.Incidents())
	}
	if cpuIncs[0].From != model.StatusOK || cpuIncs[0].To != model.StatusWarn {
		t.Fatalf("事件内容错误: %+v", cpuIncs[0])
	}
	if got, want := time.UnixMilli(cpuIncs[0].T), base.Add(t0); got.Sub(want) > time.Second {
		t.Fatalf("事件时间应为迁移发生时刻: %v want≈%v", got, want)
	}
	// 跳档：ok→warn→sat 快速连跳 → ok→warn 立即确认，warn→sat 达标后确认
	a2 := &App{prevStatus: map[string]string{}, pending: map[string]pendingInc{}}
	t2 := func(offset time.Duration, st string) {
		a2.recordTransitions(rep(st), &model.Snapshot{TS: base.Add(offset)})
	}
	t2(0, model.StatusOK)
	t2(2*time.Second, model.StatusWarn)
	t2(4*time.Second, model.StatusSat)
	cpu2 := cpuDim(a2.Incidents())
	if len(cpu2) != 1 || cpu2[0].To != model.StatusWarn {
		t.Fatalf("跳档应立即确认原迁移: %+v", a2.Incidents())
	}
	t2(4*time.Second+minDwellMs*time.Millisecond+time.Second, model.StatusSat)
	cpu2 = cpuDim(a2.Incidents())
	if len(cpu2) != 2 || cpu2[1].From != model.StatusWarn || cpu2[1].To != model.StatusSat {
		t.Fatalf("跳档后新迁移应达标确认: %+v", a2.Incidents())
	}
}

func cpuDim(incs []store.Incident) []store.Incident {
	var out []store.Incident
	for _, in := range incs {
		if in.Dim == "cpu" {
			out = append(out, in)
		}
	}
	return out
}
