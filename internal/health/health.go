// Package health 按阈值 + 滑动均值对每个资源维度给出健康结论。
// 瞬时尖峰不打扰（靠滑动窗口抹平），持续高位才判亚健康/饱和。
package health

import (
	"fmt"
	"strings"

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

// hysteresis 滞回带：升档即触发，降档需跌破 阈值-3。
// 消除均值在阈值附近 ±小幅波动导致的 ok↔warn↔sat 来回翻转（事件刷屏）。
const hysteresis = 3

type Judge struct {
	Th     Thresholds
	window int
	last   map[string]string // 各维度上一次结论（滞回的"当前档位"）
}

func NewJudge(th Thresholds, window int) *Judge {
	if window < 1 {
		window = 1
	}
	return &Judge{Th: th, window: window, last: map[string]string{}}
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

// classify 带滞回的档位判定：升档立即生效；降档需跌破下界-滞回带。
func (j *Judge) classify(key string, v, de, sat float64) string {
	cur := statusBy(v, de, sat)
	switch j.last[key] {
	case model.StatusSat:
		if cur == model.StatusWarn && v >= sat-hysteresis {
			cur = model.StatusSat
		}
	case model.StatusWarn:
		if cur == model.StatusOK && v >= de-hysteresis {
			cur = model.StatusWarn
		}
	}
	j.last[key] = cur
	return cur
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
	add(DimStatus{Key: "cpu", Label: "CPU", Status: j.classify("cpu", cpu, j.Th.CPUDegraded, j.Th.CPUSaturated), Value: cpu, Unit: "%", Detail: fmt.Sprintf("均值 %.1f%%", cpu)})
	add(DimStatus{Key: "mem", Label: "内存", Status: j.classify("mem", mem, j.Th.MemDegraded, j.Th.MemSaturated), Value: mem, Unit: "%", Detail: fmt.Sprintf("物理占用均值 %.1f%%", mem)})
	add(DimStatus{Key: "commit", Label: "提交内存", Status: j.classify("commit", commit, j.Th.CommitDegraded, j.Th.CommitSaturated), Value: commit, Unit: "%", Detail: fmt.Sprintf("提交均值 %.1f%%", commit)})

	// 磁盘：按物理盘身份键聚合均值；均值分母用该盘实际出现的次数——
	// 实例缺席的 tick 不摊薄（拔插盘/实例名重建不导致持续打满的盘漏报）。
	type dacc struct {
		busy, lat float64
		n         int
		label     string
	}
	disks := map[string]*dacc{}
	for _, r := range recs {
		for _, d := range r.Disks {
			id := model.DiskKey(d.ID)
			a := disks[id]
			if a == nil {
				a = &dacc{label: d.L}
				disks[id] = a
			}
			a.busy += d.Busy
			a.lat += d.LatMs
			a.n++
		}
	}
	for id, a := range disks {
		a.busy, a.lat = a.busy/float64(a.n), a.lat/float64(a.n)
		st := worse(j.classify("disk:"+id+":busy", a.busy, j.Th.DiskBusyDegraded, j.Th.DiskBusySaturated),
			j.classify("disk:"+id+":lat", a.lat, j.Th.DiskLatDegradedMs, j.Th.DiskLatSaturatedMs))
		add(DimStatus{
			Key: "disk:" + id, Label: "磁盘 " + pickLabel(id, a.label),
			Status: st, Value: a.busy, Unit: "%",
			Detail: fmt.Sprintf("忙 %.0f%%，读写延迟 %.1fms", a.busy, a.lat),
		})
	}

	// GPU：利用率 + 显存占用率（身份键 = LUID，稳定不随命名漂移）
	type gacc struct {
		util, vram float64
		n          int
		label      string
	}
	gpus := map[string]*gacc{}
	for _, r := range recs {
		for _, g := range r.GPUs {
			a := gpus[g.Key]
			if a == nil {
				a = &gacc{label: g.Name}
				gpus[g.Key] = a
			}
			a.util += g.Util
			a.vram += g.VramPct
			a.n++
		}
	}
	for key, a := range gpus {
		a.util, a.vram = a.util/float64(a.n), a.vram/float64(a.n)
		vramSt := model.StatusOK
		if a.vram > 0 { // 总量未知（0）时跳过显存判定
			vramSt = j.classify("gpu:"+key+":vram", a.vram, j.Th.VRAMDegraded, j.Th.VRAMSaturated)
		}
		st := worse(j.classify("gpu:"+key+":util", a.util, j.Th.GPUUtilDegraded, j.Th.GPUUtilSaturated), vramSt)
		add(DimStatus{
			Key: "gpu:" + key, Label: "GPU " + pickLabel(key, a.label),
			Status: st, Value: a.util, Unit: "%",
			Detail: fmt.Sprintf("利用率 %.0f%%", a.util),
		})
	}
	return rep
}

// ApplyDegrade 采集能力降级不能被解读为"健康"：传感器读不到的维度
// 结论保持原样，但 Overall 至少亚健康，并把降级原因列入 Reasons
// （once 的退出码随之体现）。降级发生在配置层而非 Evaluate 内，
// 使 Evaluate 保持纯函数语义（可被离线 report 复用）。
func ApplyDegrade(rep *Report, capErrs []string) {
	if len(capErrs) == 0 || rep.Overall == model.StatusSat {
		return
	}
	rep.Overall = model.StatusWarn
	rep.Reasons = append(rep.Reasons, "采集降级（部分维度不可见）："+strings.Join(capErrs, "；"))
}

func pickLabel(id, letters string) string {
	if letters != "" {
		return letters
	}
	return id
}
