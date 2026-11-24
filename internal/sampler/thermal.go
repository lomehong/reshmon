//go:build windows

package sampler

// ACPI 热区温度与 Windows Energy Meter 功耗——无驱动路径。
//
// 为什么不走"每核温度"：那需要内核驱动读 MSR（DTS/RAPL 寄存器），与本
// 工具"零依赖、不装驱动"的定位冲突。这里只取标准权限可读的 PDH 计数器。
//
// 单位不是查文档猜的，而是在本机实测并与 Windows 自带的计数器帮助文本
// （Perflib\009\Help）对照确认的：
//   - Temperature：「热区域的温度，以开氏度(Kelvin)为单位」
//   - High Precision Temperature：「以十分之开氏度(Kelvin)为单位」
//   - Energy Meter\Power：「测量每个采样间隔的平均功率(以毫瓦为单位)」；
//     计数器类型 PERF_COUNTER_BULK_COUNT，first=累计能量（皮瓦时）、
//     second=累计时间（纳小时），Δfirst/Δsecond 即毫瓦
//
// 两者都是可选增强：多数机器有，没有也不算采集降级。

import (
	"math"
	"strings"

	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/syswin"
)

const (
	pathThermalHP   = `\Thermal Zone Information(*)\High Precision Temperature`
	pathThermal     = `\Thermal Zone Information(*)\Temperature`
	pathThrottle    = `\Thermal Zone Information(*)\Throttle Reasons`
	pathEnergyPower = `\Energy Meter(*)\Power`

	kelvinOffset = 273.15
	// 有效热区读数范围（K）。固件假值常见为 0、0.1 或固定的 278.2/300K，
	// 落在范围外的直接丢弃。
	kelvinMin, kelvinMax = 200.0, 450.0
	// thermalStaleTicks 热区读数连续这么多拍都从未变化 → 疑似固件固定值。
	// 默认间隔下约 5 分钟。
	thermalStaleTicks = 150
	powerMaxW         = 2000.0 // 单域功率合理上限，超出视为坏读数
)

var thermalPaths = map[string]string{
	"hp":  pathThermalHP,
	"k":   pathThermal,
	"thr": pathThrottle,
}

// hottestKelvin 从一组热区原始值里取最热的有效读数（scale=每个单位多少 K）。
// 没有核心温度可读时，封装附近最热的那个热区最接近 CPU 实际温度。
func hottestKelvin(set rawSet, scale float64) (float64, bool) {
	best := -1.0
	for _, v := range set {
		k := float64(v.first) * scale
		if k < kelvinMin || k > kelvinMax {
			continue
		}
		if k > best {
			best = k
		}
	}
	return best, best > 0
}

// wattsFromRaw PERF_COUNTER_BULK_COUNT：Δ能量/Δ时间 = 毫瓦 → 瓦。
// 计数器回绕或重置（Δ能量为负）时返回 false，本拍无值、基线自然重建。
// Δ能量为 0 是有效的零功率，照常返回。
func wattsFromRaw(prev, now rawVal) (float64, bool) {
	dE, dT := now.first-prev.first, now.second-prev.second
	if dT <= 0 || dE < 0 {
		return 0, false
	}
	w := float64(dE) / float64(dT) / 1000
	if math.IsNaN(w) || math.IsInf(w, 0) || w > powerMaxW {
		return 0, false
	}
	return w, true
}

type powerDomain int

const (
	powerNone         powerDomain = iota // 认不出：不显示、不猜测
	powerPackage                         // RAPL *_PKG：多路（多插槽）求和
	powerPackageAlias                    // "Apu Power"/"Socket Power"：同义别名，只取其一
	powerCore                            // RAPL *_CORE：各核求和
	powerCoreAlias                       // "CPU Power"
	powerGPU                             // "GPU Power"/"GPU 0 Power"
)

// classifyPowerMeter 按 Energy Meter 实例名归入功率域。
//
// 别名之间不能相加：实测本机 "Apu Power" 与 "Socket Power" 是同一个读数
// （指同一封装）。"NPU Power"/"System Power" 等不属于封装/核心/核显口径，
// 归 None。实例命名随厂商而异，认不出的宁可不给。
func classifyPowerMeter(name string) powerDomain {
	if name == "" || name == "_Total" {
		return powerNone
	}
	n := strings.ToUpper(name)
	switch {
	case strings.HasSuffix(n, "_PKG"):
		return powerPackage
	case strings.HasPrefix(n, "RAPL_") && strings.HasSuffix(n, "_CORE"):
		return powerCore
	case n == "APU POWER" || n == "SOCKET POWER":
		return powerPackageAlias
	case n == "CPU POWER":
		return powerCoreAlias
	case strings.HasPrefix(n, "GPU") && strings.HasSuffix(n, "POWER"):
		return powerGPU
	}
	return powerNone
}

type powerReading struct{ pkg, core, gpu float64 }

// aggregatePower 按域汇总各实例的本拍功率（瓦）；无读数的域为 -1。
// 优先取 RAPL 域之和，缺测时回退到同义的别名读数。
func aggregatePower(perMeter map[string]float64) powerReading {
	r := powerReading{pkg: -1, core: -1, gpu: -1}
	pkgAlias, coreAlias := -1.0, -1.0
	add := func(dst *float64, w float64) {
		if *dst < 0 {
			*dst = 0
		}
		*dst += w
	}
	for name, w := range perMeter {
		switch classifyPowerMeter(name) {
		case powerPackage:
			add(&r.pkg, w)
		case powerCore:
			add(&r.core, w)
		case powerGPU:
			add(&r.gpu, w)
		case powerPackageAlias:
			pkgAlias = math.Max(pkgAlias, w)
		case powerCoreAlias:
			coreAlias = math.Max(coreAlias, w)
		}
	}
	if r.pkg < 0 {
		r.pkg = pkgAlias
	}
	if r.core < 0 {
		r.core = coreAlias
	}
	return r
}

// readThermal 读取 CPU 温度（ACPI 热区最热值）、节流原因与功耗。
// 不可得的项置 -1；全工具口径一致——≤0 一律视为不可得（零值安全）。
func (s *Sampler) readThermal(snap *model.Snapshot, cur map[string]rawSet) {
	cpu := &snap.CPU
	cpu.TempC, cpu.PkgW, cpu.CoreW, cpu.GpuW = -1, -1, -1, -1

	// 温度：高精度（0.1K）优先，回退 1K
	for _, c := range []struct {
		key   string
		scale float64
	}{{"hp", 0.1}, {"k", 1}} {
		set := s.rawOptional("thermal:"+c.key, s.cThermal[c.key], cur)
		if k, ok := hottestKelvin(set, c.scale); ok {
			// 按采集精度取整（高精度档 0.1K）：避免落盘出现 57.05000000000001 这类长尾
			cpu.TempC = math.Round((k-kelvinOffset)*10) / 10
			s.trackThermalChange(set)
			break
		}
	}
	cpu.ThermalStale = cpu.TempC > 0 && thermalIsStale(s.ticks, s.thermalChanged)

	// 节流原因：任一热区报告非零位掩码即视为 ACPI 报告存在节流
	if set := s.rawOptional("thermal:thr", s.cThermal["thr"], cur); set != nil {
		for _, v := range set {
			if v.first != 0 {
				cpu.Throttling = true
				break
			}
		}
	}

	// 功耗：两帧差分（速率类，首拍无值）
	set := s.rawOptional("power", s.cPower, cur)
	if set == nil {
		return
	}
	prev := s.prevRaw["power"]
	if prev == nil {
		return
	}
	per := make(map[string]float64, len(set))
	for name, now := range set {
		if p, ok := prev[name]; ok {
			if w, ok2 := wattsFromRaw(p, now); ok2 {
				per[name] = math.Round(w*100) / 100 // 毫瓦口径，保留两位小数即可
			}
		}
	}
	r := aggregatePower(per)
	cpu.PkgW, cpu.CoreW, cpu.GpuW = r.pkg, r.core, r.gpu
}

// thermalIsStale 热区读数疑似固件固定值：运行满阈值拍数却从未变化。
// 某些主板的 ACPI 热区只提供固定值（如恒为 278.2K），这种读数不能当温度用。
func thermalIsStale(ticks int, changed bool) bool {
	return ticks >= thermalStaleTicks && !changed
}

// trackThermalChange 记录热区读数自启动以来是否变过（固定值检测）。
func (s *Sampler) trackThermalChange(set rawSet) {
	for name, v := range set {
		if last, seen := s.thermalSeen[name]; seen && last != v.first {
			s.thermalChanged = true
		}
		s.thermalSeen[name] = v.first
	}
}

// rawOptional 可选计数器读取：缺失或读取出错都静默返回 nil，不记采集降级。
// 温度/功耗属于增强项——机器没有 ACPI 热区是常态，不该因此被判"采集降级"。
func (s *Sampler) rawOptional(key string, c *syswin.PDHCounter, cur map[string]rawSet) rawSet {
	if c == nil {
		return nil
	}
	items, err := c.RawItems()
	if err != nil {
		return nil
	}
	set := rawSet{}
	for _, it := range items {
		set[it.Name] = rawVal{first: it.First, second: it.Second, ts: it.TS}
	}
	cur[key] = set
	return set
}
