// Package health 按阈值 + 滑动均值对每个资源维度给出健康结论。
// 瞬时尖峰不打扰（靠滑动窗口抹平），持续高位才判亚健康/饱和。
package health

import (
	"fmt"

	"github.com/lomehong/reshmon/internal/model"
)

// Thresholds 各维度阈值（单位：百分比或毫秒）。可被 config.json 覆盖。
type Thresholds struct {
	CPUDegraded        float64 `json:"cpuDegraded"`
	CPUSaturated       float64 `json:"cpuSaturated"`
	MemDegraded        float64 `json:"memDegraded"`
	MemSaturated       float64 `json:"memSaturated"`
	CommitDegraded     float64 `json:"commitDegraded"`
	CommitSaturated    float64 `json:"commitSaturated"`
	DiskBusyDegraded   float64 `json:"diskBusyDegraded"`
	DiskBusySaturated  float64 `json:"diskBusySaturated"`
	DiskLatDegradedMs  float64 `json:"diskLatDegradedMs"`
	DiskLatSaturatedMs float64 `json:"diskLatSaturatedMs"`
	GPUUtilDegraded    float64 `json:"gpuUtilDegraded"`
	GPUUtilSaturated   float64 `json:"gpuUtilSaturated"`
	VRAMDegraded       float64 `json:"vramDegraded"`
	VRAMSaturated      float64 `json:"vramSaturated"`
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		CPUDegraded:        65,
		CPUSaturated:       85,
		MemDegraded:        85,
		MemSaturated:       92,
		CommitDegraded:     88,
		CommitSaturated:    95,
		DiskBusyDegraded:   75,
		DiskBusySaturated:  90,
		DiskLatDegradedMs:  20,
		DiskLatSaturatedMs: 45,
		GPUUtilDegraded:    80,
		GPUUtilSaturated:   95,
		VRAMDegraded:       88,
		VRAMSaturated:      96,
	}
}

type DimStatus struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Status string  `json:"status"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit,omitempty"`
	Detail string  `json:"detail,omitempty"`
}

type Report struct {
	Overall string      `json:"overall"`
	Dims    []DimStatus `json:"dims"`
	Reasons []string    `json:"reasons,omitempty"`
}

// StatusMap 供 Totals.Health 落盘：key → 状态。
func (r Report) StatusMap() map[string]string {
	m := map[string]string{"overall": r.Overall}
	for _, d := range r.Dims {
		m[d.Key] = d.Status
	}
	return m
}

type Judge struct {
	Th     Thresholds
	window int
}

func NewJudge(th Thresholds, window int) *Judge {
	if window < 1 {
		window = 1
	}
	return &Judge{Th: th, window: window}
}

// Window 判定所用的滑动窗口 tick 数。
func (j *Judge) Window() int { return j.window }

func worse(a, b string) string {
	if a == model.StatusSat || b == model.StatusSat {
		return model.StatusSat
	}
	if a == model.StatusWarn || b == model.StatusWarn {
		return model.StatusWarn
	}
	return model.StatusOK
}

func statusBy(v, de, sat float64) string {
	switch {
	case v >= sat:
		return model.StatusSat
	case v >= de:
		return model.StatusWarn
	default:
		return model.StatusOK
	}
}

// Evaluate 对最近 window 个 Totals 求均值后逐维判定。
func (j *Judge) Evaluate(recs []model.Totals) Report {
	var rep Report
	n := len(recs)
	if n == 0 {
		rep.Overall = model.StatusOK
		return rep
	}
	if n > j.window {
		recs = recs[n-j.window:]
	}
	add := func(d DimStatus) {
		rep.Dims = append(rep.Dims, d)
		rep.Overall = worse(rep.Overall, d.Status)
		if d.Status != model.StatusOK {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf("%s %s：%s", d.Label, model.StatusCN(d.Status), d.Detail))
		}
	}

	// CPU / 内存 / 提交
	var cpu, mem, commit float64
	for _, r := range recs {
		cpu += r.CPU
		mem += r.MemUsedPct
		commit += r.CommitPct
	}
	k := float64(len(recs))
	cpu, mem, commit = cpu/k, mem/k, commit/k
	add(DimStatus{Key: "cpu", Label: "CPU", Status: statusBy(cpu, j.Th.CPUDegraded, j.Th.CPUSaturated), Value: cpu, Unit: "%", Detail: fmt.Sprintf("均值 %.1f%%", cpu)})
	add(DimStatus{Key: "mem", Label: "内存", Status: statusBy(mem, j.Th.MemDegraded, j.Th.MemSaturated), Value: mem, Unit: "%", Detail: fmt.Sprintf("物理占用均值 %.1f%%", mem)})
	add(DimStatus{Key: "commit", Label: "提交内存", Status: statusBy(commit, j.Th.CommitDegraded, j.Th.CommitSaturated), Value: commit, Unit: "%", Detail: fmt.Sprintf("提交均值 %.1f%%", commit)})

	// 磁盘：按盘实例聚合均值
	type dacc struct {
		busy, lat float64
		label     string
	}
	disks := map[string]*dacc{}
	for _, r := range recs {
		for _, d := range r.Disks {
			a := disks[d.ID]
			if a == nil {
				a = &dacc{label: d.L}
				disks[d.ID] = a
			}
			a.busy += d.Busy
			a.lat += d.LatMs
		}
	}
	for id, a := range disks {
		a.busy, a.lat = a.busy/k, a.lat/k
		st := worse(statusBy(a.busy, j.Th.DiskBusyDegraded, j.Th.DiskBusySaturated),
			statusBy(a.lat, j.Th.DiskLatDegradedMs, j.Th.DiskLatSaturatedMs))
		add(DimStatus{
			Key: "disk:" + id, Label: "磁盘 " + pickLabel(id, a.label),
			Status: st, Value: a.busy, Unit: "%",
			Detail: fmt.Sprintf("忙 %.0f%%，读写延迟 %.1fms", a.busy, a.lat),
		})
	}

	// GPU：利用率 + 显存占用率
	type gacc struct {
		util, vram float64
		label      string
	}
	gpus := map[string]*gacc{}
	for _, r := range recs {
		for _, g := range r.GPUs {
			a := gpus[g.Key]
			if a == nil {
				a = &gacc{}
				gpus[g.Key] = a
			}
			a.util += g.Util
			a.vram += g.VramPct
		}
	}
	for key, a := range gpus {
		a.util, a.vram = a.util/k, a.vram/k
		vramSt := model.StatusOK
		if a.vram > 0 { // 总量未知（0）时跳过显存判定
			vramSt = statusBy(a.vram, j.Th.VRAMDegraded, j.Th.VRAMSaturated)
		}
		st := worse(statusBy(a.util, j.Th.GPUUtilDegraded, j.Th.GPUUtilSaturated), vramSt)
		add(DimStatus{
			Key: "gpu:" + key, Label: "GPU " + key,
			Status: st, Value: a.util, Unit: "%",
			Detail: fmt.Sprintf("利用率 %.0f%%", a.util),
		})
	}
	return rep
}

func pickLabel(id, letters string) string {
	if letters != "" {
		return letters
	}
	return id
}
