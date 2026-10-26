//go:build windows

package app

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/sampler"
	"github.com/lomehong/reshmon/internal/store"
)

const (
	// procTopKeep 落盘的进程切片上限（四个维度 Top 并集）。
	procTopKeep = 100
	// incKeep 内存保留的事件条数（/api/incidents 与 TUI 数据源）。
	incKeep = 1000
	// errKeep 内存保留的运行错误条数（按消息去重，磁盘满时不无限膨胀）。
	errKeep = 50
	// baselineFreshMs 重启后事件基线的新鲜度窗口：只回放最近 30 分钟的
	// 末态，太旧的末态与当前真实状态可能已隔着停机期未知变化，不作为基线。
	baselineFreshMs = 30 * 60 * 1000
)

// virtualNICTokens 总量求和时剔除的虚拟适配器名标记——Hyper-V/WSL 场景
// 同一流量会同时过 vNIC 与 pNIC 两个实例，全量求和会把网络总量翻倍。
var virtualNICTokens = []string{
	"hyper-v", "vethernet", "wsl", "tap-", "isatap", "teredo", "loopback", "qos",
}

// App 常驻运行时。loop 周期执行：Tick → 健康判定 → 事件 → 环形缓冲/落盘。
// cur/curHealth/incAll/errors 受 mu 保护（HTTP 协程与采样协程并发读写）；
// prevStatus/tickN 仅 loop 协程触碰，无需锁。
type App struct {
	Cfg    Config
	s      *sampler.Sampler
	ring   *store.Ring
	writer *store.Writer
	incLog *store.IncidentLog
	judge  *health.Judge
	lock   *dirLock

	mu         sync.RWMutex
	cur        *model.Snapshot
	curHealth  health.Report
	incAll     []store.Incident
	errors     []string
	errSeen    map[string]bool
	prevStatus map[string]string
	tickN      int

	stopCh chan struct{}
	doneCh chan struct{}
}

func Start(cfg Config) (*App, error) {
	s, err := sampler.New(cfg.Interval, cfg.Patterns)
	if err != nil {
		return nil, err
	}
	var lock *dirLock
	var lockErr error
	if cfg.Record {
		if lk, lerr := lockDataDir(cfg.DataDir); lerr == nil {
			lock = lk
		} else {
			// 双实例互斥：另一实例持有该目录的写权，本实例降级为只看。
			cfg.Record = false
			lockErr = lerr
		}
	}
	a := &App{
		Cfg:        cfg,
		s:          s,
		ring:       store.NewRing(cfg.RingCap),
		judge:      health.NewJudge(cfg.Thresholds, cfg.JudgeTicks),
		errSeen:    map[string]bool{},
		prevStatus: map[string]string{},
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		lock:       lock,
	}
	if lockErr != nil {
		a.addErr("历史记录已停用：" + lockErr.Error())
	}
	a.loadIncidentBaseline()
	if cfg.Record {
		if w, err := store.NewWriter(cfg.DataDir, cfg.ProcTicks, cfg.RetentionDays); err == nil {
			a.writer = w
		} else {
			a.addErr("历史落盘不可用：" + err.Error())
		}
		if il, err := store.OpenIncidentLog(cfg.DataDir); err == nil {
			a.incLog = il
		} else {
			a.addErr("事件日志不可用：" + err.Error())
		}
	}
	go a.loop()
	return a, nil
}

// loadIncidentBaseline 启动时回放已落盘事件：内存事件表续上历史（面板
// 时间线重启不归零），最近 30 分钟的各维度末态作为 prevStatus 基线
// （重启不再把同一持续状态重复记一次"进入"）。
func (a *App) loadIncidentBaseline() {
	incs, err := store.LoadIncidents(a.Cfg.DataDir, time.Now().Add(-7*24*time.Hour), time.Now().Add(time.Minute))
	if err != nil || len(incs) == 0 {
		return
	}
	if len(incs) > incKeep {
		incs = incs[len(incs)-incKeep:]
	}
	a.incAll = append(a.incAll, incs...)
	fresh := time.Now().UnixMilli() - baselineFreshMs
	for _, in := range incs {
		if in.T >= fresh {
			a.prevStatus[in.Dim] = in.To
		}
	}
}

func (a *App) loop() {
	defer close(a.doneCh)
	a.step(true) // 首拍建立差分基线：速率零值，不入窗口/不落盘（防均值被稀释）
	t := time.NewTicker(a.Cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-t.C:
			a.step(false)
		}
	}
}

func (a *App) step(baseline bool) {
	snap := a.s.Tick()
	if baseline {
		return
	}
	totals := TotalsFrom(snap)

	// 判定窗口 = 环形尾部 + 当前
	window := a.judge.Window()
	seed := make([]model.Totals, 0, window)
	seed = append(seed, a.ring.Last(window-1)...)
	seed = append(seed, totals)
	rep := a.judge.Evaluate(seed)
	health.ApplyDegrade(&rep, snap.CapErrors)
	totals.Health = rep.StatusMap()

	a.recordTransitions(rep, snap)
	a.ring.Push(totals)

	if a.writer != nil {
		if err := a.writer.Append(totals); err != nil {
			a.addErr("落盘失败：" + err.Error())
		}
		a.tickN++
		if a.tickN%a.Cfg.ProcTicks == 0 {
			top := store.TrimProcs(snap.Procs, procTopKeep)
			if err := a.writer.AppendProcs(snap.TS, top); err != nil {
				a.addErr("进程切片落盘失败：" + err.Error())
			}
		}
	}

	a.mu.Lock()
	a.cur, a.curHealth = snap, rep
	a.mu.Unlock()
}

func (a *App) recordTransitions(rep health.Report, snap *model.Snapshot) {
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
			// 内存事件始终记录（-no-record 时时间线才可用）；落盘按配置。
			a.mu.Lock()
			a.incAll = append(a.incAll, inc)
			if len(a.incAll) > incKeep {
				a.incAll = a.incAll[len(a.incAll)-incKeep:]
			}
			a.mu.Unlock()
			a.incLog.Append(inc)
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

// Incidents 最近 incKeep 条状态迁移（内存副本——含启动时回放的历史，
// -no-record 模式下也有当前会话的事件）。
func (a *App) Incidents() []store.Incident {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]store.Incident, len(a.incAll))
	copy(out, a.incAll)
	return out
}

// addErr 记录运行错误：按消息去重、封顶 errKeep（磁盘满等持续故障
// 恰好发生在用户最需要面板的时刻，不能让错误表吃光内存）。
func (a *App) addErr(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.errSeen[msg] {
		return
	}
	a.errSeen[msg] = true
	a.errors = append(a.errors, msg)
	if len(a.errors) > errKeep {
		a.errors = a.errors[len(a.errors)-errKeep:]
	}
}

// AddErr 外部（如命令入口注入配置告警）记录运行错误。
func (a *App) AddErr(msg string) { a.addErr(msg) }

// Errs 返回运行错误（只读拷贝）。
func (a *App) Errs() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.errors))
	copy(out, a.errors)
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
	a.lock.release()
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
			// 身份键用 LUID（稳定）；DXGI 命名失败只影响显示名，
			// 不再让同一块 GPU 的健康维度/事件序列一分为二。
			Key: g.LUIDKey, Name: g.Name, Util: g.UtilCapPct, VramPct: GPUVramPct(g),
		})
	}
	nr, ns := NicTotals(s.NICs)
	t.NetRecvBps, t.NetSentBps = nr, ns
	return t
}

// NicTotals 网络收发总量（剔除虚拟适配器，防 vNIC/pNIC 双计翻倍）。
func NicTotals(nics []model.NICStat) (recv, sent float64) {
	for _, n := range nics {
		if isVirtualNIC(n.Name) {
			continue
		}
		recv += n.RecvBps
		sent += n.SentBps
	}
	return recv, sent
}

func isVirtualNIC(name string) bool {
	name = strings.ToLower(name)
	for _, tok := range virtualNICTokens {
		if strings.Contains(name, tok) {
			return true
		}
	}
	return false
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
