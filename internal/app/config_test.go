package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 回归：配置文件写坏（哪怕一个尾逗号）曾被完全静默吞掉——用户改阈值
// 不生效且零提示。现在必须返回告警并继续用默认值运行。
func TestLoadConfigBrokenJSONWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"intervalMs": 1000,,}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(`{"agents": [}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, warns := LoadConfig(dir)
	if len(warns) < 2 {
		t.Fatalf("坏 config.json 与 agents.json 都应告警: %v", warns)
	}
	if cfg.Interval != 2*time.Second {
		t.Fatalf("坏配置应回落默认间隔: %v", cfg.Interval)
	}
	if cfg.Thresholds.CPUDegraded != DefaultConfig().Thresholds.CPUDegraded {
		t.Fatal("坏配置应回落默认阈值")
	}
}

// 首次运行写出的模板必须带完整阈值（曾被写成 "thresholds": null，
// 用户想覆盖阈值得自己猜结构）。
func TestConfigTemplateHasThresholds(t *testing.T) {
	dir := t.TempDir()
	_, warns := LoadConfig(dir)
	for _, w := range warns {
		t.Fatalf("首运行不应告警: %s", w)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	th, ok := m["thresholds"].(map[string]any)
	if !ok || th["cpuDegraded"] == nil {
		t.Fatalf("模板缺少可用 thresholds: %s", b)
	}
	if !strings.Contains(string(b), "procEverySec") {
		t.Fatalf("模板缺少 procEverySec: %s", b)
	}
}

func TestLoadConfigOverride(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"intervalMs": 500, "procEverySec": 60, "retentionDays": 7, "thresholds": {"cpuDegraded": 70}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, warns := LoadConfig(dir)
	if len(warns) != 0 {
		t.Fatalf("合法配置不应告警: %v", warns)
	}
	if cfg.Interval != 500*time.Millisecond {
		t.Fatalf("间隔覆盖失败: %v", cfg.Interval)
	}
	if cfg.ProcEverySec != 60 {
		t.Fatalf("procEverySec 覆盖失败: %d", cfg.ProcEverySec)
	}
	if cfg.RetentionDays != 7 {
		t.Fatal("retentionDays 覆盖失败")
	}
	if cfg.Thresholds.CPUDegraded != 70 || cfg.Thresholds.CPUSaturated != DefaultConfig().Thresholds.CPUSaturated {
		t.Fatal("阈值应逐项覆盖、其余保持默认")
	}
}

// 时长基准：间隔调小后，30s 判定窗口 / 30s 切片 / 2h 环形缓冲的 tick 数
// 随之放大——README 的时长承诺不随间隔漂移。
func TestDeriveIntervalScaling(t *testing.T) {
	cfg := DefaultConfig()
	cfg.derive()
	if cfg.JudgeTicks != 15 || cfg.ProcTicks != 15 || cfg.RingCap != 3600 { // 2s 默认
		t.Fatalf("默认派生错误: %+v", cfg)
	}
	cfg.Interval = 500 * time.Millisecond
	cfg.derive()
	if cfg.JudgeTicks != 60 {
		t.Fatalf("500ms 间隔判定窗口应为 60 tick: %d", cfg.JudgeTicks)
	}
	if cfg.ProcTicks != 60 {
		t.Fatalf("500ms 间隔切片应为 60 tick: %d", cfg.ProcTicks)
	}
	if cfg.RingCap != 14400 {
		t.Fatalf("500ms 间隔环形缓冲应为 14400 tick: %d", cfg.RingCap)
	}
}
