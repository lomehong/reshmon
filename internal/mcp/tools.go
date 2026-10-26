//go:build windows

package mcp

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

func toolSnapshot(src Source, args json.RawMessage) (any, error) {
	var a struct {
		IncludeProcesses bool `json:"includeProcesses"`
	}
	_ = json.Unmarshal(args, &a)
	snap, rep := src.Current()
	if snap == nil {
		return map[string]any{"starting": true, "hint": "首拍采样中，约 1-2 秒后重试"}, nil
	}
	out := map[string]any{
		"ts":        snap.TS.Format(time.RFC3339),
		"overall":   rep.Overall,
		"cpu":       map[string]any{"busyPct": round1(snap.CPU.BusyPct), "cores": snap.CPU.Cores},
		"memory":    map[string]any{"usedPct": round1(snap.Mem.PhysUsedPct), "commitPct": round1(snap.Mem.CommitPct), "availableBytes": snap.Mem.PhysAvailB},
		"disks":     map[string]any{},
		"gpus":      map[string]any{},
		"network":   map[string]any{},
		"capErrors": snap.CapErrors,
		"errors":    src.Errs(),
	}
	if len(rep.Reasons) > 0 {
		out["reasons"] = rep.Reasons
	}
	for _, d := range snap.Disks {
		out["disks"].(map[string]any)[d.Letters] = map[string]any{
			"busyPct": round1(d.BusyPct), "readBps": uint64(d.ReadBps), "writeBps": uint64(d.WriteBps),
			"latencyMs": round1((d.ReadLatMs + d.WriteLatMs) / 2),
		}
	}
	for _, g := range snap.GPUs {
		out["gpus"].(map[string]any)[g.Name] = map[string]any{
			"utilPct": round1(g.UtilCapPct), "vramPct": round1(vramPct(g)), "tempC": g.TempC,
		}
	}
	var nr, ns float64
	for _, n := range snap.NICs {
		nr += n.RecvBps
		ns += n.SentBps
	}
	out["network"] = map[string]any{"recvBps": uint64(nr), "sentBps": uint64(ns)}
	if a.IncludeProcesses {
		procs := append([]model.ProcStat(nil), snap.Procs...)
		sort.Slice(procs, func(i, j int) bool { return procs[i].CpuPct > procs[j].CpuPct })
		if len(procs) > 20 {
			procs = procs[:20]
		}
		out["topProcesses"] = procRows(procs)
	}
	return out, nil
}

func vramPct(g model.GPUStat) float64 {
	if g.DedicatedTotalB == 0 {
		return 0
	}
	v := float64(g.DedicatedUsedB) / float64(g.DedicatedTotalB) * 100
	if v > 100 {
		return 100
	}
	return v
}

func procRows(procs []model.ProcStat) []map[string]any {
	rows := make([]map[string]any, 0, len(procs))
	for _, p := range procs {
		rows = append(rows, map[string]any{
			"pid": p.PID, "name": p.Name, "agent": p.Agent,
			"cpuPct": round1(p.CpuPct), "memPrivateBytes": p.MemPrivB,
			"ioBps": uint64(p.ReadBps + p.WriteBps), "gpuPct": round1(p.GpuPct), "vramBytes": p.VramB,
		})
	}
	return rows
}

func toolAgentUsage(src Source) (any, error) {
	snap, _ := src.Current()
	if snap == nil {
		return nil, errors.New("采样尚未就绪，稍后重试")
	}
	rows := make([]map[string]any, 0, len(snap.Agents))
	for _, a := range snap.Agents {
		rows = append(rows, map[string]any{
			"agent": a.Name, "cpuPct": round1(a.CpuPct), "memPrivateBytes": a.MemPrivB,
			"ioBps": uint64(a.ReadBps + a.WriteBps), "gpuPct": round1(a.GpuPct),
			"vramBytes": a.VramB, "processes": a.Procs,
		})
	}
	return map[string]any{"agents": rows, "ts": snap.TS.Format(time.RFC3339)}, nil
}

func toolTopProcesses(src Source, args json.RawMessage) (any, error) {
	var a struct {
		Dimension string `json:"dimension"`
		Limit     int    `json:"limit"`
	}
	_ = json.Unmarshal(args, &a)
	if a.Dimension == "" {
		a.Dimension = "cpu"
	}
	if a.Limit <= 0 {
		a.Limit = 15
	}
	if a.Limit > 50 {
		a.Limit = 50
	}
	snap, _ := src.Current()
	if snap == nil {
		return nil, errors.New("采样尚未就绪，稍后重试")
	}
	procs := append([]model.ProcStat(nil), snap.Procs...)
	key := func(p model.ProcStat) float64 {
		switch a.Dimension {
		case "mem":
			return float64(p.MemPrivB)
		case "io":
			return p.ReadBps + p.WriteBps
		case "gpu":
			return p.GpuPct
		default:
			return p.CpuPct
		}
	}
	sort.Slice(procs, func(i, j int) bool { return key(procs[i]) > key(procs[j]) })
	if len(procs) > a.Limit {
		procs = procs[:a.Limit]
	}
	return map[string]any{"dimension": a.Dimension, "processes": procRows(procs)}, nil
}

func toolTrend(src Source, args json.RawMessage) (any, error) {
	var a struct {
		Minutes int `json:"minutes"`
	}
	_ = json.Unmarshal(args, &a)
	if a.Minutes <= 0 {
		a.Minutes = 10
	}
	if a.Minutes > 120 {
		a.Minutes = 120
	}
	ticks := src.Ring().Last(int(float64(a.Minutes*60)/src.Interval().Seconds()) + 4)
	cutoff := time.Now().Add(-time.Duration(a.Minutes) * time.Minute).UnixMilli()
	recs := make([]model.Totals, 0, len(ticks))
	for _, t := range ticks {
		if t.T >= cutoff {
			recs = append(recs, t)
		}
	}
	if len(recs) == 0 {
		return map[string]any{"samples": 0, "hint": "环形缓冲尚无覆盖该窗口的数据"}, nil
	}
	acc := func(get func(model.Totals) float64) map[string]any {
		var sum, max float64
		for i, r := range recs {
			v := get(r)
			sum += v
			if i == 0 || v > max {
				max = v
			}
		}
		return map[string]any{"avg": round1(sum / float64(len(recs))), "max": round1(max)}
	}
	out := map[string]any{
		"samples":    len(recs),
		"from":       time.UnixMilli(recs[0].T).Format(time.RFC3339),
		"to":         time.UnixMilli(recs[len(recs)-1].T).Format(time.RFC3339),
		"cpu":        acc(func(t model.Totals) float64 { return t.CPU }),
		"memory":     acc(func(t model.Totals) float64 { return t.MemUsedPct }),
		"commit":     acc(func(t model.Totals) float64 { return t.CommitPct }),
		"disks":      map[string]any{},
		"gpus":       map[string]any{},
		"netRecvBps": acc(func(t model.Totals) float64 { return t.NetRecvBps }),
		"netSentBps": acc(func(t model.Totals) float64 { return t.NetSentBps }),
	}
	perDisk := map[string][]float64{}
	for _, r := range recs {
		for _, d := range r.Disks {
			perDisk[model.DiskKey(d.ID)+" "+d.L] = append(perDisk[model.DiskKey(d.ID)+" "+d.L], d.Busy)
		}
	}
	for name, vs := range perDisk {
		var sum, max float64
		for i, v := range vs {
			sum += v
			if i == 0 || v > max {
				max = v
			}
		}
		out["disks"].(map[string]any)[name] = map[string]any{"busyAvgPct": round1(sum / float64(len(vs))), "busyMaxPct": round1(max)}
	}
	perGPU := map[string][]float64{}
	for _, r := range recs {
		for _, g := range r.GPUs {
			label := g.Name
			if label == "" {
				label = g.Key
			}
			perGPU[label] = append(perGPU[label], g.Util)
		}
	}
	for name, vs := range perGPU {
		var sum, max float64
		for i, v := range vs {
			sum += v
			if i == 0 || v > max {
				max = v
			}
		}
		out["gpus"].(map[string]any)[name] = map[string]any{"utilAvgPct": round1(sum / float64(len(vs))), "utilMaxPct": round1(max)}
	}
	return out, nil
}

func toolEvents(src Source, args json.RawMessage) (any, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	_ = json.Unmarshal(args, &a)
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}
	incs := src.Incidents()
	rows := make([]map[string]any, 0, len(incs))
	for _, in := range incs {
		rows = append(rows, map[string]any{
			"time": time.UnixMilli(in.T).Format(time.RFC3339),
			"dim":  in.Dim, "from": in.From, "to": in.To, "detail": in.Detail,
		})
	}
	// 最新在前
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	if len(rows) > a.Limit {
		rows = rows[:a.Limit]
	}
	return map[string]any{"events": rows}, nil
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
