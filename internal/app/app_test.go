//go:build windows

package app

import (
	"sync"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/model"
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

// -no-record 模式下事件时间线仍然工作（incLog 为 nil 不再整体早退）。
func TestNoRecordIncidentsLive(t *testing.T) {
	a := &App{prevStatus: map[string]string{}}
	okRep := health.Report{Overall: model.StatusOK, Dims: []health.DimStatus{{Key: "cpu", Label: "CPU", Status: model.StatusOK}}}
	warnRep := health.Report{Overall: model.StatusWarn, Dims: []health.DimStatus{{Key: "cpu", Label: "CPU", Status: model.StatusWarn, Detail: "均值 70.0%"}}}
	snap := &model.Snapshot{TS: time.Now()}
	a.recordTransitions(okRep, snap)   // 建基线：无事件
	a.recordTransitions(warnRep, snap) // ok→warn：必须入内存时间线
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
		t.Fatalf("no-record 模式事件不入时间线: %+v", incs)
	}
}
