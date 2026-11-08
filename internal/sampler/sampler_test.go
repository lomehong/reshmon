//go:build windows

package sampler

import (
	"sort"
	"testing"

	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/syswin"
)

func TestParseGPUEngineInstance(t *testing.T) {
	pid, luid, ok := ParseGPUEngineInstance("pid_12540_luid_0x00000000_0x00013C5E_phys_0_eng_0_engtype_3D")
	if !ok || pid != 12540 {
		t.Fatalf("pid 解析失败: pid=%d luid=%s ok=%v", pid, luid, ok)
	}
	if want := syswin.LUIDKey(0x00000000, 0x00013C5E); luid != want {
		t.Fatalf("luid 归一化失败: %s != %s", luid, want)
	}
	if _, _, ok := ParseGPUEngineInstance("luid_0x00000000_0x00013C5E_phys_0"); ok {
		t.Fatal("无 pid 实例不应匹配")
	}
	if _, _, ok := ParseGPUEngineInstance("junk"); ok {
		t.Fatal("垃圾输入不应匹配")
	}
}

func TestParseLUIDInstance(t *testing.T) {
	luid, ok := ParseLUIDInstance("pid_1_luid_0x00000000_0x00013C5E_phys_0")
	if !ok || luid != "0x00000000_0x00013c5e" {
		t.Fatalf("解析失败: %s %v", luid, ok)
	}
	if _, ok := ParseLUIDInstance("no-luid-here"); ok {
		t.Fatal("垃圾输入不应匹配")
	}
}

func TestRateHelpers(t *testing.T) {
	a := rawVal{first: 1000, second: 10, ts: 10000000}
	b := rawVal{first: 2000, second: 20, ts: 20000000}
	if v := timerPct(a, b); v < 0.0099 || v > 0.0101 { // Δ1000 / Δ1e7 ×100 = 0.01%
		t.Fatalf("timerPct 口径变化: %v", v)
	}
	if r := bulkRate(a, b); r < 999 || r > 1001 { // Δ1000 字节 / Δ1 秒 = 1000/s
		t.Fatalf("bulkRate 口径变化: %v", r)
	}
	if ms := avgTimerMs(a, b, 1e7); ms < 0.0099 || ms > 0.0101 { // Δ1000×100ns/Δ10次 = 0.01ms/次
		t.Fatalf("avgTimerMs 口径变化: %v", ms)
	}
	if v := timerPct(b, a); v != 0 { // 时间倒流防护
		t.Fatalf("倒流应返回 0: %v", v)
	}
	// 反相计时器：原始值累计空闲 → 活跃 = 100 - 空闲
	if v := invTimerPct(a, b); v < 99.98 || v > 99.991 { // 空闲 0.01% → 活跃 99.99%
		t.Fatalf("invTimerPct 口径变化: %v", v)
	}
}

// PERF_AVERAGE_TIMER 必须按计数器真实 timebase 换算：QPC≠10MHz 的平台
// 上固定除 1e7 会把延迟放大 1e7/base 倍。
func TestAvgTimerTimeBase(t *testing.T) {
	a := rawVal{first: 0, second: 0, ts: 10000000}
	b := rawVal{first: 45960, second: 16, ts: 20000000} // 实测样例（10MHz）
	if ms := avgTimerMs(a, b, 1e7); ms < 0.287 || ms > 0.288 {
		t.Fatalf("10MHz 时基换算错误: %v", ms)
	}
	// 3.579545MHz（老 HPET）：同样计数对应更长的真实时间
	old := avgTimerMs(a, b, 3.579545e6)
	if old < 0.80 || old > 0.82 {
		t.Fatalf("非 10MHz 时基换算错误: %v", old)
	}
	if avgTimerMs(a, b, 0) != 0 {
		t.Fatal("零时基应返回 0")
	}
}

// 锁定「计数器键 → 差分口径」接线：任何一处把口径接错（如把反相当正相）
// 都应在此失败——929c96b 修的正是这类接线错误。
func TestTimerOrRawWiring(t *testing.T) {
	// 1s 观察窗（Δts=1e7 个 100ns），累计值增加 9e6：计时器口径=90%，速率口径=9e6/s
	a := rawVal{first: 0, second: 0, ts: 10000000}
	b := rawVal{first: 9000000, second: 20, ts: 20000000}

	// idle 为正相计时器：差分 = 空闲% (90)
	if v := timerOrRaw("idle", a, b, 1e7); v < 89.9 || v > 90.1 {
		t.Fatalf("idle 口径错误（应为空闲 90%%）: %v", v)
	}
	// q 为瞬时原始值
	if v := timerOrRaw("q", a, rawVal{first: 7}, 1e7); v != 7 {
		t.Fatalf("q 应取瞬时值: %v", v)
	}
	// rms/wms 走 avgTimerMs：9e6/1e7×1000/20 次 = 45ms/次
	if v := timerOrRaw("rms", a, b, 1e7); v < 44.9 || v > 45.1 {
		t.Fatalf("rms 口径错误: %v", v)
	}
	// rb/wb/rps/wps 走 bulkRate：9e6 字节 / 1s
	if v := timerOrRaw("rb", a, b, 1e7); v < 8.999e6 || v > 9.001e6 {
		t.Fatalf("rb 口径错误: %v", v)
	}
}

func TestLettersOf(t *testing.T) {
	if l := lettersOf("0 C:"); l != "C:" {
		t.Fatalf("lettersOf 失败: %q", l)
	}
	if l := lettersOf("1 D: E:"); l != "D: E:" {
		t.Fatalf("lettersOf 多盘符失败: %q", l)
	}
	if l := lettersOf("2"); l != "" {
		t.Fatalf("lettersOf 无盘符应为空: %q", l)
	}
}

// WARP（Microsoft Basic Render Driver）必须被识别剔除——否则其偶发的
// 引擎活动会凭空多出一张 "GPU 0x0000…" 幻影卡片（实测复现过的 bug）。
func TestWarpLUIDSet(t *testing.T) {
	ads := []syswin.DXGIAdapter{
		{LUIDKey: "0x00000000_0x0000c437", Description: "AMD Radeon(TM) 860M Graphics", Vendor: "AMD"},
		{LUIDKey: "0x00000000_0x000000e1", Description: "Microsoft Basic Render Driver", Vendor: "Microsoft", IsWARP: true},
	}
	w := warpLUIDSet(ads)
	if !w["0x00000000_0x000000e1"] {
		t.Fatal("WARP 适配器未被标记")
	}
	if w["0x00000000_0x0000c437"] {
		t.Fatal("物理 GPU 不得被标记为 WARP")
	}
	if len(warpLUIDSet(nil)) != 0 {
		t.Fatal("空适配器表应返回空集")
	}
}

// CPU 缺口比例归因：按采样占比分配，免校准；边界（零缺口/零样本/空池）返回 nil。
func TestSplitGapCPU(t *testing.T) {
	split := splitGapCPU(60, map[uint32]uint64{100: 300, 200: 100})
	if len(split) != 2 || split[100] != 45 || split[200] != 15 {
		t.Fatalf("比例拆分错误: %+v", split)
	}
	// 单个进程独占缺口
	split = splitGapCPU(90, map[uint32]uint64{7: 12})
	if split[7] != 90 {
		t.Fatalf("独占缺口错误: %+v", split)
	}
	// 零缺口 / 零样本 / 空池
	if splitGapCPU(0, map[uint32]uint64{1: 5}) != nil {
		t.Fatal("零缺口应返回 nil")
	}
	if splitGapCPU(-3, map[uint32]uint64{1: 5}) != nil {
		t.Fatal("负缺口应返回 nil")
	}
	if splitGapCPU(50, map[uint32]uint64{1: 0}) != nil {
		t.Fatal("零样本应返回 nil")
	}
	if splitGapCPU(50, nil) != nil {
		t.Fatal("空池应返回 nil")
	}
}

// 跨开机稳定身份键：设备名优先；同名多卡加序号；命名回退卡用 LUID。
func TestAssignStableKeys(t *testing.T) {
	gpus := []model.GPUStat{
		{LUIDKey: "0xb_0x2", Name: "AMD Radeon(TM) 860M Graphics"},
		{LUIDKey: "0xa_0x1", Name: "AMD Radeon(TM) 860M Graphics"}, // 同名双卡（排序在前 → 不带序号）
		{LUIDKey: "0xc_0x3", Name: "GPU 0xc_0x3"},                  // DXGI 命名失败的回退卡
		{LUIDKey: "0xd_0x4", Name: "NVIDIA GeForce RTX 4090"},
	}
	// readGPUs 里先按 LUIDKey 排序再分配
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].LUIDKey < gpus[j].LUIDKey })
	assignStableKeys(gpus)
	want := map[string]string{
		"0xa_0x1": "AMD Radeon(TM) 860M Graphics",
		"0xb_0x2": "AMD Radeon(TM) 860M Graphics#2",
		"0xc_0x3": "0xc_0x3", // 命名回退卡：键用裸 LUID（与历史落盘键一致）
		"0xd_0x4": "NVIDIA GeForce RTX 4090",
	}
	for _, g := range gpus {
		if g.StableKey != want[g.LUIDKey] {
			t.Fatalf("LUID %s 稳定键 = %q want %q", g.LUIDKey, g.StableKey, want[g.LUIDKey])
		}
	}
}
