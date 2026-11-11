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
	"github.com/lomehong/reshmon/internal/syswin"
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
	etw    *syswin.EphemeralTracker

	mu         sync.RWMutex
	cur        *model.Snapshot
	curHealth  health.Report
	incAll     []store.Incident
	errors     []string
	errSeen    map[string]bool
	prevStatus map[string]string
	pending    map[string]pendingInc // 挂起中的状态迁移（防抖确认期）
	lastTickAt time.Time             // 上一个成功 tick 的时刻（冻结看门狗）
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
		pending:    map[string]pendingInc{},
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		lock:       lock,
	}
	if lockErr != nil {
		a.addErr("历史记录已停用：" + lockErr.Error())
	}
	if cfg.ETW {
		// 短命进程归因（ETW）：需要管理员，失败只降级不阻断。
		// IO 走 DiskIO 事件精确配对；CPU 走 SampledProfile 采样比例归因。
		if tr, terr := syswin.StartEphemeralTracker(cfg.ETWCPU); terr == nil {
			a.etw = tr
			s.AttachEphemeral(tr)
			if cfg.ETWCPU && !tr.CPUActive() {
				a.addErr("CPU 采样归因未启用（SeSystemProfilePrivilege 特权不可用），IO 归因正常")
			}
		} else {
			a.addErr("短命进程归因（ETW）不可用：" + terr.Error())
		}
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
	a.scanRecorderGaps()
	go a.loop()
	return a, nil
}

// scanRecorderGaps 启动时扫描最近 48h 的 totals 断档，把历史冻结补成
// "recorder 冻结" 事件（按起点时刻去重）——数据缺失区间在时间线上
// 诚实可见，而不是让人误以为冻结期间一切正常。
func (a *App) scanRecorderGaps() {
	from := time.Now().Add(-48 * time.Hour)
	gaps, err := store.ScanGaps(a.Cfg.DataDir, from, time.Now())
	if err != nil {
		a.addErr("断档扫描失败：" + err.Error())
		return
	}
	if len(gaps) == 0 {
		return
	}
	existing := map[int64]bool{}
	if incs, err := store.LoadIncidents(a.Cfg.DataDir, from, time.Now()); err == nil {
		for _, in := range incs {
			if in.Dim == "recorder" {
				existing[in.T] = true
			}
		}
	}
	for _, g := range gaps {
		if !existing[g.T] {
			a.addIncident(g)
		}
	}
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

// stallIncident 采样管线冻结的自证事件：tick 间隔超过阈值即管线曾停摆
// （磁盘卡死把阻塞写挂住是最常见的诱因）。恢复后的第一个成功 tick
// 立刻记录冻结时长——「丢了多少」本身成为时间线上的一条事实。
func stallIncident(lastTick, now time.Time) (store.Incident, bool) {
	gap := now.Sub(lastTick)
	if gap < store.RecorderStallMs {
		return store.Incident{}, false
	}
	return store.Incident{
		T:    lastTick.UnixMilli(),
		Dim:  "recorder",
		From: "ok",
		To:   "stalled",
		Detail: fmt.Sprintf("采样管线冻结 %.0f 分钟后自愈（%s 起数据缺失，冻结期间系统状态未被观测）",
			gap.Minutes(), lastTick.Format("15:04:05")),
	}, true
}

func (a *App) step(baseline bool) {
	snap := a.s.Tick()
	if baseline {
		return
	}
	// 看门狗自证：距上一成功 tick 超过阈值 = 管线刚从冻结中恢复
	if inc, hit := stallIncident(a.lastTickAt, snap.TS); hit {
		a.addIncident(inc)
	}
	a.lastTickAt = snap.TS
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

// minDwellMs 状态迁移确认的最小驻留时长：迁移发生后须在新档位停留
// 达此时长才落事件；期间回到原状态视为抖动脉冲直接丢弃，跳到第三档
// 则立即确认原迁移并挂起新迁移。11-06 实测一天 78 条事件多为秒级往返
// 脉冲，此机制把时间线还给真实持续的异常。事件时间戳仍是迁移首次
// 观察时刻（防抖只延迟确认，不改时间）。语义常量归 store（重建端共用）。
const minDwellMs = store.MinDwellMs

// pendingInc 挂起中的迁移（仅 loop 协程触碰，无需锁）。
type pendingInc struct {
	inc     store.Incident // T = 迁移首次观察时刻
	sinceMs int64
}

func (a *App) recordTransitions(rep health.Report, snap *model.Snapshot) {
	cur := rep.StatusMap()
	nowMs := snap.TS.UnixMilli()
	var confirmed []store.Incident
	for key, st := range cur {
		if p, has := a.pending[key]; has {
			switch {
			case st == p.inc.From:
				delete(a.pending, key) // 回到原状态：抖动脉冲，整体丢弃（prevStatus 不动）
			case st != p.inc.To:
				// 跳到第三档（如 ok→warn 途中冲到 sat）：原迁移成立，新迁移挂起
				confirmed = append(confirmed, p.inc)
				a.pending[key] = pendingInc{
					inc:     store.Incident{T: nowMs, Dim: key, From: p.inc.To, To: st, Detail: dimDetail(rep, key)},
					sinceMs: nowMs,
				}
			case nowMs-p.sinceMs >= minDwellMs:
				confirmed = append(confirmed, p.inc) // 驻留达标：确认
				delete(a.pending, key)
				a.prevStatus[key] = st
			}
			continue
		}
		if prev, had := a.prevStatus[key]; had {
			if prev != st {
				a.pending[key] = pendingInc{
					inc:     store.Incident{T: nowMs, Dim: key, From: prev, To: st, Detail: dimDetail(rep, key)},
					sinceMs: nowMs,
				}
			}
			continue
		}
		a.prevStatus[key] = st // 首次观察：建立基线，不产生事件
	}
	for _, inc := range confirmed {
		a.addIncident(inc)
	}
}

// addIncident 统一落事件：内存时间线（-no-record 时也可用）+ 文件 +
// 首次写失败可见化（v0.5.0 曾静默失败 24h——丢失本身必须被看见）。
func (a *App) addIncident(inc store.Incident) {
	a.mu.Lock()
	a.incAll = append(a.incAll, inc)
	if len(a.incAll) > incKeep {
		a.incAll = a.incAll[len(a.incAll)-incKeep:]
	}
	a.mu.Unlock()
	a.incLog.Append(inc)
	if n, errMsg := a.incLog.Failures(); n == 1 {
		a.addErr("事件日志写入失败，自动重试中：" + errMsg)
	}
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

// Interval 采样间隔（MCP 等消费者换算窗口 tick 数用）。
func (a *App) Interval() time.Duration { return a.Cfg.Interval }

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
	if a.etw != nil {
		a.etw.Stop()
	}
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
			LatMs:   (d.ReadLatMs + d.WriteLatMs) / 2,
			FreePct: d.FreePct,
		})
	}
	for _, g := range s.GPUs {
		key := g.StableKey
		if key == "" {
			key = g.LUIDKey // 防御：未分配稳定键时退回 LUID
		}
		t.GPUs = append(t.GPUs, model.GPUTotal{
			// 身份键用跨开机稳定的设备名键（LUID 每次开机重新生成，会让
			// 健康维度/事件序列在重启后断裂）；DXGI 命名失败只影响显示名。
			Key: key, Name: g.Name, Util: g.UtilCapPct, VramPct: GPUVramPct(g),
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
