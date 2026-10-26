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
		if d.Key == "disk:0 C:" && d.Status == model.StatusSat {
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
