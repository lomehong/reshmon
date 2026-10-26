package health

import (
	"testing"

	"github.com/lomehong/reshmon/internal/model"
)

func mk(cpu, mem, commit float64) model.Totals {
	return model.Totals{CPU: cpu, MemUsedPct: mem, CommitPct: commit}
}

func TestEvaluateCPULevels(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 3)
	rep := j.Evaluate([]model.Totals{mk(90, 30, 30), mk(91, 30, 30), mk(92, 30, 30)})
	if rep.Overall != model.StatusSat {
		t.Fatalf("持续 90+ 应饱和: %s", rep.Overall)
	}
	rep = j.Evaluate([]model.Totals{mk(70, 30, 30), mk(70, 30, 30), mk(70, 30, 30)})
	if rep.Overall != model.StatusWarn {
		t.Fatalf("持续 70 应亚健康: %s", rep.Overall)
	}
	rep = j.Evaluate([]model.Totals{mk(50, 30, 30), mk(50, 30, 30), mk(50, 30, 30)})
	if rep.Overall != model.StatusOK {
		t.Fatalf("持续 50 应健康: %s", rep.Overall)
	}
}

func TestSpikeSuppressed(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 5)
	// 单帧尖峰被窗口均值抹平
	rep := j.Evaluate([]model.Totals{mk(20, 30, 30), mk(20, 30, 30), mk(99, 30, 30), mk(20, 30, 30), mk(20, 30, 30)})
	if rep.Overall != model.StatusOK {
		t.Fatalf("单帧尖峰不应告警: %s", rep.Overall)
	}
}

func TestWindowTailOnly(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 3)
	// 前 10 帧低，最后 3 帧高 → 只看尾部
	rep := j.Evaluate([]model.Totals{
		mk(1, 1, 1), mk(1, 1, 1), mk(1, 1, 1), mk(1, 1, 1), mk(1, 1, 1),
		mk(1, 1, 1), mk(1, 1, 1), mk(1, 1, 1), mk(1, 1, 1), mk(1, 1, 1),
		mk(95, 1, 1), mk(96, 1, 1), mk(97, 1, 1),
	})
	if rep.Overall != model.StatusSat {
		t.Fatalf("尾部高位应饱和: %s", rep.Overall)
	}
}

func TestDiskDims(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 2)
	recs := []model.Totals{
		{Disks: []model.DiskTotal{{ID: "0 C:", L: "C:", Busy: 96, LatMs: 2}}},
		{Disks: []model.DiskTotal{{ID: "0 C:", L: "C:", Busy: 94, LatMs: 3}}},
	}
	rep := j.Evaluate(recs)
	found := false
	for _, d := range rep.Dims {
		// 身份键 = 物理盘号（"0 C:" → "0"）
		if d.Key == "disk:0" && d.Status == model.StatusSat {
			found = true
		}
	}
	if !found {
		t.Fatalf("磁盘饱和维度缺失: %+v", rep.Dims)
	}
	if rep.Overall != model.StatusSat {
		t.Fatalf("整体应随磁盘饱和: %s", rep.Overall)
	}
}

func TestStatusMap(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 1)
	m := j.Evaluate([]model.Totals{mk(90, 30, 30)}).StatusMap()
	if m["overall"] != model.StatusSat || m["cpu"] != model.StatusSat {
		t.Fatalf("StatusMap 错误: %v", m)
	}
}

// 边界语义锁定：>= 阈值即触发（值=阈值 / 阈值±0.1）。
func TestBoundaryValues(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		v    float64
		de   float64
		sat  float64
		want string
	}{
		{th.CPUDegraded, th.CPUDegraded, th.CPUSaturated, model.StatusWarn},
		{th.CPUDegraded - 0.1, th.CPUDegraded, th.CPUSaturated, model.StatusOK},
		{th.CPUSaturated, th.CPUDegraded, th.CPUSaturated, model.StatusSat},
		{th.CPUSaturated - 0.1, th.CPUDegraded, th.CPUSaturated, model.StatusWarn},
		{0, th.CPUDegraded, th.CPUSaturated, model.StatusOK},
		{100, th.CPUDegraded, th.CPUSaturated, model.StatusSat},
	}
	for i, c := range cases {
		j := NewJudge(th, 1) // 滞回有状态：每个用例用全新 Judge
		if got := j.classify("cpu", c.v, c.de, c.sat); got != c.want {
			t.Fatalf("case %d: classify(%v) = %s want %s", i, c.v, got, c.want)
		}
	}
}

// 滞回：升档立即生效；均值回落到阈值下方但未跌破 阈值-3 时保持原档，
// 防止 ok↔warn↔sat 在阈值附近抖动刷爆事件时间线。
func TestHysteresis(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 1)
	th := DefaultThresholds()
	// 进入 warn（65）
	if got := j.classify("cpu", 66, th.CPUDegraded, th.CPUSaturated); got != model.StatusWarn {
		t.Fatalf("升档失败: %s", got)
	}
	// 回落到 63（< 65 但 >= 62）→ 保持 warn
	if got := j.classify("cpu", 63, th.CPUDegraded, th.CPUSaturated); got != model.StatusWarn {
		t.Fatalf("滞回带内不应降档: %s", got)
	}
	// 跌破 62 → 降为 ok
	if got := j.classify("cpu", 61, th.CPUDegraded, th.CPUSaturated); got != model.StatusOK {
		t.Fatalf("跌破滞回带应降档: %s", got)
	}
	// sat 同理
	j2 := NewJudge(DefaultThresholds(), 1)
	if got := j2.classify("cpu", 90, th.CPUDegraded, th.CPUSaturated); got != model.StatusSat {
		t.Fatalf("升档失败: %s", got)
	}
	if got := j2.classify("cpu", 84, th.CPUDegraded, th.CPUSaturated); got != model.StatusSat {
		t.Fatalf("sat 滞回带内不应降档: %s", got)
	}
	if got := j2.classify("cpu", 81, th.CPUDegraded, th.CPUSaturated); got != model.StatusWarn {
		t.Fatalf("跌破 sat 滞回带应降为 warn: %s", got)
	}
}

// 实例缺席的 tick 不摊薄均值：某盘只在窗口内出现部分 tick 且持续打满，
// 均值应按出现次数计算（否则拔插盘后打满的盘会被稀释成"健康"）。
func TestInstancePresenceDilution(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 15)
	var recs []model.Totals
	for i := 0; i < 15; i++ {
		recs = append(recs, mk(10, 30, 30))
	}
	// 只在 5 个 tick 出现、每次都 100% 忙
	for i := 0; i < 5; i++ {
		recs[i].Disks = []model.DiskTotal{{ID: "1 D:", L: "D:", Busy: 100, LatMs: 60}}
	}
	rep := j.Evaluate(recs)
	found := false
	for _, d := range rep.Dims {
		if d.Key == "disk:1" {
			found = true
			if d.Status != model.StatusSat {
				t.Fatalf("按出现次数均值应饱和: %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("磁盘维度缺失")
	}
}

// 磁盘身份键 = 物理盘号：盘符增减（"1 D:" → "1 D: E:"）应是同一维度，
// 不产生假事件、不拆裂序列。
func TestDiskKeyStable(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 2)
	recs := []model.Totals{
		{Disks: []model.DiskTotal{{ID: "1 D:", L: "D:", Busy: 96}}},
		{Disks: []model.DiskTotal{{ID: "1 D: E:", L: "D: E:", Busy: 94}}},
	}
	rep := j.Evaluate(recs)
	count := 0
	for _, d := range rep.Dims {
		if len(d.Key) > 5 && d.Key[:5] == "disk:" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同一物理盘被拆成 %d 个维度", count)
	}
}

// 采集降级不能伪装成健康：Overall 至少 warn 且原因入列。
func TestApplyDegrade(t *testing.T) {
	j := NewJudge(DefaultThresholds(), 1)
	rep := j.Evaluate([]model.Totals{mk(10, 30, 30)})
	if rep.Overall != model.StatusOK {
		t.Fatal("前置失败")
	}
	ApplyDegrade(&rep, []string{"GPU 计数器不可用"})
	if rep.Overall != model.StatusWarn {
		t.Fatalf("降级应至少 warn: %s", rep.Overall)
	}
	if len(rep.Reasons) == 0 {
		t.Fatal("降级原因未入 Reasons")
	}
	// 已饱和的不动；无降级信息的不动
	sat := j.Evaluate([]model.Totals{mk(99, 99, 99)})
	ApplyDegrade(&sat, []string{"x"})
	if sat.Overall != model.StatusSat {
		t.Fatal("饱和不受降级影响")
	}
	ok := j.Evaluate([]model.Totals{mk(10, 30, 30)})
	ApplyDegrade(&ok, nil)
	if ok.Overall != model.StatusOK {
		t.Fatal("无降级信息不应改变结论")
	}
}
