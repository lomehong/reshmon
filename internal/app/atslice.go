//go:build windows

package app

// 进程概览环形缓冲与"某一时刻"回溯。
//
// 面板上的趋势图是每拍总量，而"当时是谁在吃资源"需要进程级数据。
// 落盘的进程切片是每 ProcTicks 拍（默认 30s）一份且要读盘；这里在内存里
// 留精简概览，点击趋势图任意时刻即可就地回溯。
//
// 两个约束决定了存储方式：
//   - 覆盖时长必须不短于趋势图最长档（2 小时）——曲线上任何一点点下去都
//     要有数据，不能点了后半段提示"超出范围"；
//   - 内存要可控：靠抽稀（每 sliceEvery 拍存一份）把槽位数固定在 sliceCap，
//     占用与采样间隔无关。Agent 账本不在这里另存——总量环形缓冲每拍都有。
//
// 数据缺失区间（记录器冻结/睡眠）如实返回 blind，而不是给一个看似正常的
// 空结果——这正是 v0.5.x 数据丢失事故的教训。

import (
	"math"
	"sort"
	"time"

	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

const (
	// sliceSpan 概览覆盖的时长，与趋势图最长档一致。
	sliceSpan = 2 * time.Hour
	// sliceCap 概览环形缓冲的槽位数（固定值）。
	sliceCap = 1200
	// sliceKeep 每份概览保留的进程数：CPU/内存/IO/GPU 四个维度 Top 的并集
	// （store.TrimProcs）。只按 CPU 取 Top 的话，按"磁盘忙"回溯时重 IO
	// 的进程可能根本不在名单里。
	sliceKeep = 48
	// sliceGapMs 相邻两份概览超过此时长即视为数据缺失（与冻结看门狗同口径）。
	sliceGapMs = 60_000
)

// sliceProc 一份概览里的一个进程（只留回溯需要的字段，控制内存）。
type sliceProc struct {
	Name     string  `json:"n"`
	Agent    string  `json:"a,omitempty"`
	PID      uint32  `json:"pid"`
	CpuPct   float64 `json:"cpu"`
	MemPrivB uint64  `json:"priv"`
	ReadBps  float64 `json:"rb,omitempty"`
	WriteBps float64 `json:"wb,omitempty"`
	GpuPct   float64 `json:"gpu,omitempty"`
}

// procSlice 某一时刻的进程概览。
type procSlice struct {
	T      int64       `json:"t"`
	CPU    float64     `json:"cpu"`
	MemPct float64     `json:"memp"`
	Procs  []sliceProc `json:"procs"`
}

// sliceEveryFor 每隔多少拍存一份概览，使 sliceCap 个槽位恰好覆盖 sliceSpan。
// 间隔 2s 时每 3 拍（6 秒）一份；间隔 500ms 时每 12 拍一份——时间粒度
// 基本恒定，内存占用也恒定。
func sliceEveryFor(interval time.Duration) int {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticks := int(sliceSpan / interval)
	if ticks <= sliceCap {
		return 1
	}
	return (ticks + sliceCap - 1) / sliceCap
}

// buildProcSlice 从快照生成一份概览：四维度 Top 并集，按 CPU 降序。
func buildProcSlice(snap *model.Snapshot, keep int) procSlice {
	top := store.TrimProcs(snap.Procs, keep)
	ps := make([]sliceProc, 0, len(top))
	for _, p := range top {
		ps = append(ps, sliceProc{
			Name: p.Name, Agent: p.Agent, PID: p.PID, CpuPct: p.CpuPct,
			MemPrivB: p.MemPrivB, ReadBps: p.ReadBps, WriteBps: p.WriteBps, GpuPct: p.GpuPct,
		})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].CpuPct > ps[j].CpuPct })
	return procSlice{
		T: snap.TS.UnixMilli(), CPU: snap.CPU.BusyPct, MemPct: snap.Mem.PhysUsedPct,
		Procs: ps,
	}
}

// pushProcSlice 抽稀后写入一份概览（环形，覆盖最旧）。首份立即存。
func (a *App) pushProcSlice(snap *model.Snapshot) {
	if a.sliceTick > 0 {
		a.sliceTick--
		return
	}
	s := buildProcSlice(snap, sliceKeep)
	a.sliceTick = sliceEveryFor(a.Cfg.Interval) - 1
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.procSlices == nil {
		a.procSlices = make([]procSlice, sliceCap)
	}
	a.procSlices[a.sliceAt] = s
	a.sliceAt = (a.sliceAt + 1) % len(a.procSlices)
}

// procSliceSnapshot 取出全部概览（时间升序拷贝），供回溯查询。
func (a *App) procSliceSnapshot() []procSlice {
	a.mu.RLock()
	defer a.mu.RUnlock()
	n := len(a.procSlices)
	if n == 0 {
		return nil
	}
	out := make([]procSlice, 0, n)
	// sliceAt 指向下一个写入位，即最旧的一条；从这里绕一圈即为时间升序
	for i := 0; i < n; i++ {
		s := a.procSlices[(a.sliceAt+i)%n]
		if s.T == 0 {
			continue // 尚未填满时的空位
		}
		out = append(out, s)
	}
	return out
}

// AtResult "某一时刻"回溯结果。
type AtResult struct {
	// Blind 为真表示所请求时刻落在数据缺失区间内（记录器冻结/系统睡眠）
	// 或超出保留范围，此时不给切片并如实说明——而不是返回空列表让人
	// 误读成"当时没进程"。
	Blind  bool       `json:"blind,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Slice  *procSlice `json:"slice,omitempty"`
	// Agents 该时刻的 Agent 账本（取自总量缓冲，每拍都有）。
	Agents []model.AgentStat `json:"agents,omitempty"`
	// DeltaMs 所请求时刻与返回切片的时间差（毫秒）
	DeltaMs int64 `json:"deltaMs"`
}

// At 回溯 t 时刻的进程概览：返回时间上最接近的一份。
// t 落在两份之间的缺失区间（间隔超过 sliceGapMs）时如实标记 blind。
// 范围判定留"一份概览的间距 + 一个采样间隔"的容差：点击趋势图最右端
// （最新一份之后一点）属于正常用法，不能判成"超出保留范围"。
func (a *App) At(t time.Time) AtResult {
	slices := a.procSliceSnapshot()
	if len(slices) == 0 {
		return AtResult{Blind: true, Reason: "尚无进程概览（服务刚启动）"}
	}
	spacing := time.Duration(sliceEveryFor(a.Cfg.Interval)) * a.Cfg.Interval
	if spacing <= 0 {
		spacing = time.Minute
	}
	tol := spacing + a.Cfg.Interval
	earliest, latest := slices[0].T, slices[len(slices)-1].T
	tms := t.UnixMilli()
	if tms < earliest-tol.Milliseconds() || tms > latest+tol.Milliseconds() {
		return AtResult{Blind: true, Reason: "该时刻不在内存保留范围内（面板保留最近约 2 小时；更早的数据请用 report 回溯落盘历史）"}
	}
	// 二分找最近的一份
	i := sort.Search(len(slices), func(i int) bool { return slices[i].T >= tms })
	switch {
	case i <= 0:
		i = 0
	case i >= len(slices):
		i = len(slices) - 1
	default:
		if tms-slices[i-1].T <= slices[i].T-tms {
			i--
		}
	}
	// 落在缺失区间：与最近一份的时间差超过阈值即判盲区
	delta := tms - slices[i].T
	if math.Abs(float64(delta)) > sliceGapMs {
		return AtResult{Blind: true, DeltaMs: delta,
			Reason: "该时刻附近存在数据缺失（记录器冻结或系统睡眠），这一时段的状态未被观测"}
	}
	s := slices[i]
	res := AtResult{Slice: &s, DeltaMs: delta}
	if tt := a.totalsNear(s.T); tt != nil {
		res.Agents = tt.Agents
	}
	return res
}

// totalsNear 从总量环形缓冲取时间上最接近 tms 的一拍。Agent 账本每拍都有，
// 且该缓冲覆盖 2 小时，回溯不必在进程概览里再存一份。
func (a *App) totalsNear(tms int64) *model.Totals {
	if a.ring == nil {
		return nil
	}
	all := a.ring.Last(a.ring.Len())
	if len(all) == 0 {
		return nil
	}
	i := sort.Search(len(all), func(i int) bool { return all[i].T >= tms })
	switch {
	case i <= 0:
		i = 0
	case i >= len(all):
		i = len(all) - 1
	default:
		if tms-all[i-1].T <= all[i].T-tms {
			i--
		}
	}
	return &all[i]
}
