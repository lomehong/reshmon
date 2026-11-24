//go:build windows

package sampler

import (
	"math"
	"testing"
)

// 换算回归：测试数据取自本机探针实测的 PDH RAW 值，期望值取自 Windows
// 官方格式化值（PdhGetFormattedCounterValue）。任何单位/公式改动都会在此暴露。
func TestWattsFromRaw(t *testing.T) {
	// RAPL_Package0_PKG，相邻两帧；Windows 官方值 45558.4326 mW
	prev := rawVal{first: 651387287600000, second: 4227390855}
	now := rawVal{first: 651412852350000, second: 4227951997}
	w, ok := wattsFromRaw(prev, now)
	if !ok {
		t.Fatal("有效差分应有读数")
	}
	if math.Abs(w-45.5584326) > 0.001 {
		t.Fatalf("封装功率换算偏离 Windows 官方值: %.4f W（官方 45.5584 W）", w)
	}

	// 核 0：官方 2896.0287 mW
	prevC := rawVal{first: 35513716008333, second: 4227390855}
	nowC := rawVal{first: 35515341091666, second: 4227951997}
	if w, ok = wattsFromRaw(prevC, nowC); !ok || math.Abs(w-2.8960287) > 0.001 {
		t.Fatalf("核心功率换算错误: %.4f W ok=%v", w, ok)
	}

	// 计数器回绕/重置（Δ能量为负）→ 无值，由调用方重建基线
	if _, ok := wattsFromRaw(now, prev); ok {
		t.Fatal("Δ能量为负不应给出读数")
	}
	// 时间未前进 → 无值（防除零）
	if _, ok := wattsFromRaw(rawVal{first: 1, second: 5}, rawVal{first: 2, second: 5}); ok {
		t.Fatal("Δ时间为零不应给出读数")
	}
	// 能量未增长是有效的零功率
	if w, ok := wattsFromRaw(rawVal{first: 7, second: 5}, rawVal{first: 7, second: 9}); !ok || w != 0 {
		t.Fatalf("零能量增量应返回有效零值: %v %v", w, ok)
	}
}

// 热区读数选择：高精度为十分之一开尔文，取最热的有效热区；越界假值丢弃。
func TestHottestKelvin(t *testing.T) {
	// 本机实测：High Precision=3302 → 330.2K；Temperature=330 → 330K
	if k, ok := hottestKelvin(rawSet{`\_TZ.TZ01`: {first: 3302}}, 0.1); !ok || math.Abs(k-330.2) > 1e-9 {
		t.Fatalf("高精度读数换算错误: %v %v", k, ok)
	}
	if k, ok := hottestKelvin(rawSet{`\_TZ.TZ01`: {first: 330}}, 1); !ok || k != 330 {
		t.Fatalf("普通精度读数换算错误: %v %v", k, ok)
	}
	// 多热区取最热
	set := rawSet{"TZ0": {first: 3100}, "TZ1": {first: 3455}, "TZ2": {first: 3200}}
	if k, _ := hottestKelvin(set, 0.1); math.Abs(k-345.5) > 1e-9 {
		t.Fatalf("应取最热热区: %v", k)
	}
	// 固件假值（0、0.1K、超大）丢弃；全部无效则不可得
	bad := rawSet{"a": {first: 0}, "b": {first: 1}, "c": {first: 99999}}
	if _, ok := hottestKelvin(bad, 0.1); ok {
		t.Fatal("全是越界假值时不应给出读数")
	}
	if _, ok := hottestKelvin(nil, 0.1); ok {
		t.Fatal("空集合不应给出读数")
	}
	// 混合时只取有效的那一个（浮点比较用容差：3302×0.1 不精确等于 330.2）
	mixed := rawSet{"bad": {first: 0}, "good": {first: 3302}}
	if k, ok := hottestKelvin(mixed, 0.1); !ok || math.Abs(k-330.2) > 1e-9 {
		t.Fatalf("应跳过假值取有效读数: %v %v", k, ok)
	}
}

// 功率域归类：实例名来自本机实测；认不出的不给（不猜测）。
func TestClassifyPowerMeter(t *testing.T) {
	cases := map[string]powerDomain{
		"RAPL_Package0_PKG":        powerPackage,
		"RAPL_Package1_PKG":        powerPackage,
		"RAPL_Package0_Core0_CORE": powerCore,
		"RAPL_Package0_Core7_CORE": powerCore,
		"Apu Power":                powerPackageAlias,
		"Socket Power":             powerPackageAlias,
		"CPU Power":                powerCoreAlias,
		"GPU Power":                powerGPU,
		"GPU 0 Power":              powerGPU,
		"_Total":                   powerNone,
		"NPU Power":                powerNone,
		"System Power":             powerNone,
		"":                         powerNone,
	}
	for name, want := range cases {
		if got := classifyPowerMeter(name); got != want {
			t.Fatalf("classifyPowerMeter(%q) = %d want %d", name, got, want)
		}
	}
}

// 按域汇总：RAPL 域之和优先，别名只作回退且互不相加。
// 输入为本机实测的 Windows 官方值（毫瓦→瓦）。
func TestAggregatePower(t *testing.T) {
	per := map[string]float64{
		"RAPL_Package0_PKG":        45.558,
		"RAPL_Package0_Core0_CORE": 2.896,
		"CPU Power":                25.762,
		"GPU Power":                8.796,
		"Apu Power":                48.788,
		"Socket Power":             48.788,
		"System Power":             33.797,
	}
	r := aggregatePower(per)
	if math.Abs(r.pkg-45.558) > 1e-9 {
		t.Fatalf("封装应取 RAPL 域而非别名: %v", r.pkg)
	}
	if math.Abs(r.core-2.896) > 1e-9 {
		t.Fatalf("核心应取 RAPL 各核之和: %v", r.core)
	}
	if math.Abs(r.gpu-8.796) > 1e-9 {
		t.Fatalf("核显取值错误: %v", r.gpu)
	}

	// 没有 RAPL 域：回退别名；"Apu Power" 与 "Socket Power" 是同一读数，不能相加
	r = aggregatePower(map[string]float64{"Apu Power": 48.788, "Socket Power": 48.788, "CPU Power": 25.762})
	if math.Abs(r.pkg-48.788) > 1e-9 {
		t.Fatalf("同义别名不得相加: %v", r.pkg)
	}
	if math.Abs(r.core-25.762) > 1e-9 {
		t.Fatalf("核心别名回退错误: %v", r.core)
	}
	if r.gpu != -1 {
		t.Fatalf("无核显读数应为 -1: %v", r.gpu)
	}

	// 多插槽：RAPL 封装求和
	r = aggregatePower(map[string]float64{"RAPL_Package0_PKG": 30, "RAPL_Package1_PKG": 30, "Apu Power": 999})
	if math.Abs(r.pkg-60) > 1e-9 {
		t.Fatalf("多插槽封装应求和: %v", r.pkg)
	}

	// 空输入全为 -1
	r = aggregatePower(nil)
	if r.pkg != -1 || r.core != -1 || r.gpu != -1 {
		t.Fatalf("空输入应全部不可得: %+v", r)
	}
}

// 固定值检测：某些主板的 ACPI 热区恒返回同一个值（如 278.2K = 5℃），
// 这种读数不能当温度用——运行满阈值拍数且从未变化即判定为疑似固定值。
func TestThermalStale(t *testing.T) {
	if thermalIsStale(thermalStaleTicks-1, false) {
		t.Fatal("未到阈值拍数不应判为固定值")
	}
	if !thermalIsStale(thermalStaleTicks, false) {
		t.Fatal("满阈值拍数且从未变化应判为固定值")
	}
	if thermalIsStale(thermalStaleTicks*100, true) {
		t.Fatal("读数变化过就不是固定值")
	}

	// trackThermalChange：首次见到不构成变化，之后值不同才标记
	s := &Sampler{thermalSeen: map[string]int64{}}
	s.trackThermalChange(rawSet{`\_TZ.TZ01`: {first: 2782}})
	if s.thermalChanged {
		t.Fatal("首次读数不应标记 changed")
	}
	s.trackThermalChange(rawSet{`\_TZ.TZ01`: {first: 2782}})
	if s.thermalChanged {
		t.Fatal("读数相同不应标记 changed")
	}
	s.trackThermalChange(rawSet{`\_TZ.TZ01`: {first: 3302}})
	if !s.thermalChanged {
		t.Fatal("读数变化后应标记 changed")
	}
}
