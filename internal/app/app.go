//go:build windows

package app

import (
	"fmt"
	"sync"
	"time"

	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/sampler"
	"github.com/lomehong/reshmon/internal/store"
)

// procTopKeep 落盘的进程切片上限（四个维度 Top 并集）。
const procTopKeep = 100

// App 常驻运行时。loop 周期执行：Tick → 健康判定 → 事件 → 环形缓冲/落盘。
type App struct {
	Cfg    Config
	s      *sampler.Sampler
	ring   *store.Ring
	writer *store.Writer
	incLog *store.IncidentLog
	judge  *health.Judge

	mu         sync.RWMutex
	cur        *model.Snapshot
	curHealth  health.Report
	prevStatus map[string]string
	tickN      int
	incTail    []store.Incident

	Errors []string
	stopCh chan struct{}
	doneCh chan struct{}
}

func Start(cfg Config) (*App, error) {
	s, err := sampler.New(cfg.Interval, cfg.Patterns)
	if err != nil {
		return nil, err
	}
	a := &App{
		Cfg:        cfg,
		s:          s,
		ring:       store.NewRing(cfg.RingCap),
		judge:      health.NewJudge(cfg.Thresholds, 15),
		prevStatus: map[string]string{},
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	if cfg.Record {
		if w, err := store.NewWriter(cfg.DataDir, cfg.ProcEvery, cfg.RetentionDays); err == nil {
			a.writer = w
		} else {
			a.Errors = append(a.Errors, "历史落盘不可用："+err.Error())
		}
		if il, err := store.OpenIncidentLog(cfg.DataDir); err == nil {
			a.incLog = il
		} else {
			a.Errors = append(a.Errors, "事件日志不可用："+err.Error())
		}
	}
	go a.loop()
	return a, nil
}

func (a *App) loop() {
	defer close(a.doneCh)
	a.step() // 首拍建立基线（速率零值）
	t := time.NewTicker(a.Cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-t.C:
			a.step()
		}
	}
}

func (a *App) step() {
	snap := a.s.Tick()
	totals := TotalsFrom(snap)

	// 判定窗口 = 环形尾部 + 当前
	window := a.judge.Window()
	seed := make([]model.Totals, 0, window)
	seed = append(seed, a.ring.Last(window-1)...)
	seed = append(seed, totals)
	rep := a.judge.Evaluate(seed)
	totals.Health = rep.StatusMap()

	a.recordTransitions(rep, snap)
	a.ring.Push(totals)

	if a.writer != nil {
		if err := a.writer.Append(totals); err != nil {
			a.Errors = append(a.Errors, "落盘失败："+err.Error())
		}
		a.tickN++
		if a.tickN%a.Cfg.ProcEvery == 0 {
			top := store.TrimProcs(snap.Procs, procTopKeep)
			if err := a.writer.AppendProcs(snap.TS, top); err != nil {
				a.Errors = append(a.Errors, "进程切片落盘失败："+err.Error())
			}
		}
	}

	a.mu.Lock()
	a.cur, a.curHealth = snap, rep
	a.mu.Unlock()
}

func (a *App) recordTransitions(rep health.Report, snap *model.Snapshot) {
	if a.incLog == nil {
		return
	}
	cur := rep.StatusMap()
	for key, st := range cur {
		if prev, had := a.prevStatus[key]; had && prev != st {
			inc := store.Incident{
				T:      snap.TS.UnixMilli(),
				Dim:    key,
				From:   prev,
				To:     st,
				Detail: dimDetail(rep, key),
			}
			a.incLog.Append(inc)
			a.incTail = append(a.incTail, inc)
			if len(a.incTail) > 20 {
				a.incTail = a.incTail[len(a.incTail)-20:]
			}
		}
	}
	a.prevStatus = cur
}

func dimDetail(rep health.Report, key string) string {
	for _, d := range rep.Dims {
		if d.Key == key {
			return fmt.Sprintf("%s（%s）", d.Label, d.Detail)
		}
	}
	return key
}

// Current 返回最近一次快照与健康结论（只读拷贝）。
func (a *App) Current() (*model.Snapshot, health.Report) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cur == nil {
		return nil, a.curHealth
	}
	cp := *a.cur
	return &cp, a.curHealth
}

func (a *App) Ring() *store.Ring { return a.ring }

// Incidents 最近 20 条状态迁移（内存副本，未开记录时也可用）。
func (a *App) Incidents() []store.Incident {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]store.Incident, len(a.incTail))
	copy(out, a.incTail)
	return out
}

// WaitClose 停止循环并释放资源。
func (a *App) Close() {
	close(a.stopCh)
	<-a.doneCh
	a.s.Close()
	if a.writer != nil {
		a.writer.Close()
	}
	a.incLog.Close()
}

// TotalsFrom 把快照压缩为每 tick 总量记录。
func TotalsFrom(s *model.Snapshot) model.Totals {
	t := model.Totals{
		T:          s.TS.UnixMilli(),
		CPU:        s.CPU.BusyPct,
		MemUsedPct: s.Mem.PhysUsedPct,
		CommitPct:  s.Mem.CommitPct,
		Agents:     s.Agents,
	}
	for _, d := range s.Disks {
		t.Disks = append(t.Disks, model.DiskTotal{
			ID: d.ID, L: d.Letters, Busy: d.BusyPct,
			RBps: d.ReadBps, WBps: d.WriteBps,
			LatMs: (d.ReadLatMs + d.WriteLatMs) / 2,
		})
	}
	for _, g := range s.GPUs {
		t.GPUs = append(t.GPUs, model.GPUTotal{
			Key: g.Name, Util: g.UtilCapPct, VramPct: GPUVramPct(g),
		})
	}
	var nr, ns float64
	for _, n := range s.NICs {
		nr += n.RecvBps
		ns += n.SentBps
	}
	t.NetRecvBps, t.NetSentBps = nr, ns
	return t
}

// GPUVramPct 显存占用率：独立显存口径；核显（总量 0）返回 0（不判定）。
func GPUVramPct(g model.GPUStat) float64 {
	if g.DedicatedTotalB == 0 {
		return 0
	}
	v := float64(g.DedicatedUsedB) / float64(g.DedicatedTotalB) * 100
	if v > 100 {
		return 100
	}
	return v
}
