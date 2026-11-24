//go:build windows

package app

// 进程切片环形缓冲与"某一时刻"回溯。
//
// 面板上的趋势图是每拍总量，而"当时是谁在吃资源"需要进程级数据。
// 落盘的进程切片是每 ProcTicks 拍（默认 30s）一份，粒度太粗且要读盘；
// 这里在内存里每拍留一份精简概览，点击趋势图任意时刻即可就地回溯。
// 数据缺失区间（记录器冻结/睡眠）如实返回 blind，而不是给一个看似正常
// 的空结果——这正是 v0.5.x 数据丢失事故的教训。

import (
	"math"
	"sort"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

const (
	// ringProcs 内存保留的进程切片拍数（默认间隔下约 1 小时，与趋势图
	// 最长档 2 小时相比偏短——用 ringSpanSec 按间隔换算，见 ringProcsFor）
	ringProcsBase = 3600
	// sliceTopN 每拍保留的进程条数（按 CPU 取 Top，足够回答"当时是谁"）
	sliceTopN = 40
	// sliceGapMs 相邻两拍超过此时长即视为数据缺失（与冻结看门狗同口径）
	sliceGapMs = 60_000
)

// sliceProc 一拍的进程概览（只留回溯需要的字段，控制内存）。
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

// procSlice 一拍进程概览：该时刻最忙的进程 + 该拍的 Agent 账本。
type procSlice struct {
	T      int64             `json:"t"`
	CPU    float64           `json:"cpu"`
	MemPct float64           `json:"memp"`
	Procs  []sliceProc       `json:"procs"`
	Agents []model.AgentStat `json:"agents,omitempty"`
}

// ringProcsFor 按采样间隔换算环形缓冲拍数，使覆盖时长恒定（默认约 1 小时）。
func ringProcsFor(interval time.Duration) int {
	if interval <= 0 {
		return ringProcsBase
	}
	n := int(time.Hour / interval)
	if n < 60 {
		n = 60
	}
	if n > ringProcsBase*4 {
		n = ringProcsBase * 4
	}
	return n
}

// buildProcSlice 从快照生成一拍概览：按 CPU 取 Top N。
func buildProcSlice(snap *model.Snapshot, topN int) procSlice {
	ps := make([]sliceProc, 0, len(snap.Procs))
	for _, p := range snap.Procs {
		ps = append(ps, sliceProc{
			Name: p.Name, Agent: p.Agent, PID: p.PID, CpuPct: p.CpuPct,
			MemPrivB: p.MemPrivB, ReadBps: p.ReadBps, WriteBps: p.WriteBps, GpuPct: p.GpuPct,
		})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].CpuPct > ps[j].CpuPct })
	if topN > 0 && len(ps) > topN {
		ps = ps[:topN]
	}
	return procSlice{
		T: snap.TS.UnixMilli(), CPU: snap.CPU.BusyPct, MemPct: snap.Mem.PhysUsedPct,
		Procs: ps, Agents: snap.Agents,
	}
}

// pushProcSlice 写入一拍概览（环形，覆盖最旧）。
func (a *App) pushProcSlice(snap *model.Snapshot) {
	s := buildProcSlice(snap, sliceTopN)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.procSlices == nil {
		a.procSlices = make([]procSlice, ringProcsFor(a.Cfg.Interval))
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
	// 从最旧到最新：sliceAt 指向下一个写入位，即最旧的一条
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
	// Blind 为真表示所请求时刻落在数据缺失区间内（记录器冻结/系统睡眠），
	// 此时 Procs 为空且如实说明，而不是返回空列表让人误读成"当时没进程"。
	Blind  bool       `json:"blind,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Slice  *procSlice `json:"slice,omitempty"`
	// DeltaMs 所请求时刻与返回切片的时间差（毫秒）
	DeltaMs int64 `json:"deltaMs"`
}

// At 回溯 t 时刻的进程概览：返回时间上最接近的一拍。
// 若 t 落在两拍之间的缺失区间（间隔超过 sliceGapMs），如实标记 blind。
// 范围判定留一个采样间隔的容差：点击趋势图最右端（最新一拍之后一点）
// 属于正常用法，不能判成"超出保留范围"。
func (a *App) At(t time.Time) AtResult {
	slices := a.procSliceSnapshot()
	if len(slices) == 0 {
		return AtResult{Blind: true, Reason: "尚无进程切片（服务刚启动或未开启记录）"}
	}
	earliest := time.UnixMilli(slices[0].T)
	latest := time.UnixMilli(slices[len(slices)-1].T)
	tol := a.Cfg.Interval
	if tol <= 0 || tol > time.Minute {
		tol = time.Minute
	}
	if t.Before(earliest.Add(-tol)) || t.After(latest.Add(tol)) {
		return AtResult{Blind: true, Reason: "该时刻不在内存保留范围内（面板仅保留最近约 1 小时；更早的数据请用 report 回溯落盘历史）"}
	}
	// 二分找最近的一拍
	i := sort.Search(len(slices), func(i int) bool { return slices[i].T >= t.UnixMilli() })
	switch {
	case i == 0:
		i = 0
	case i >= len(slices):
		i = len(slices) - 1
	default:
		prev := slices[i-1]
		cur := slices[i]
		if t.UnixMilli()-prev.T <= cur.T-t.UnixMilli() {
			i--
		}
	}
	// 落在缺失区间：与返回切片的时间差超过阈值即判盲区
	delta := t.UnixMilli() - slices[i].T
	if math.Abs(float64(delta)) > sliceGapMs {
		return AtResult{Blind: true, DeltaMs: delta,
			Reason: "该时刻附近存在数据缺失（记录器冻结或系统睡眠），这一时段的状态未被观测"}
	}
	s := slices[i]
	return AtResult{Slice: &s, DeltaMs: delta}
}
