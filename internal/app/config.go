// Package app 常驻运行时：定时采样 → 环形缓冲/落盘 → 健康判定 → 事件记录，
// 并承载配置装载。live/serve/once 共用。
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lomehong/reshmon/internal/attrib"
	"github.com/lomehong/reshmon/internal/health"
)

// Version 工具版本（唯一出处，CLI 与面板共用）。
const Version = "0.2.0"

// 时长基准（窗口/频率以时间为源，间隔改变时承诺不漂移）：
const (
	// JudgeWindowSec 健康判定滑动窗口时长。
	JudgeWindowSec = 30
	// ProcSliceSec 进程切片落盘间隔（时长）。
	ProcSliceSec = 30
	// RingSpan 环形缓冲覆盖时长（面板趋势用）。
	RingSpan = 2 * time.Hour
)

type Config struct {
	DataDir string
	// Interval 采样间隔（也作为 once 的观察窗基准）。
	Interval time.Duration
	// ProcEverySec 每 N 秒落一次全量进程切片（换算成 tick 在 Start 内完成）。
	ProcEverySec int
	// JudgeTicks / ProcTicks 由 Interval 派生，调用方无需设置。
	JudgeTicks int
	ProcTicks  int
	// RingCap 环形缓冲容量（tick 数，由 RingSpan 派生）。
	RingCap       int
	RetentionDays int
	Thresholds    health.Thresholds
	Patterns      []attrib.Pattern
	Record        bool
	// ETW 用内核事件补齐短命进程的 IO 归因（需要管理员；不可用时自动降级）。
	ETW bool
}

// DefaultDataDir 默认数据目录：%LOCALAPPDATA%\reshmon。
func DefaultDataDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "reshmon")
}

func DefaultConfig() Config {
	return Config{
		DataDir:       DefaultDataDir(),
		Interval:      2 * time.Second,
		ProcEverySec:  ProcSliceSec,
		RetentionDays: 14,
		Thresholds:    health.DefaultThresholds(),
		Patterns:      attrib.DefaultPatterns(),
		Record:        true,
		ETW:           true,
	}
}

// derive 按间隔换算 tick 数：判定窗口 30s、进程切片 30s、环形缓冲 2h。
// 间隔调小后各项承诺（README 中的 30s/2h）不随之缩水。
func (c *Config) derive() {
	ticks := func(d time.Duration, min int) int {
		n := int(d / c.Interval)
		if n < min {
			n = min
		}
		return n
	}
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	c.JudgeTicks = ticks(JudgeWindowSec*time.Second, 3)
	if c.ProcEverySec < 1 {
		c.ProcEverySec = ProcSliceSec
	}
	c.ProcTicks = ticks(time.Duration(c.ProcEverySec)*time.Second, 1)
	c.RingCap = ticks(RingSpan, 60)
}

// DeriveForInterval 命令行覆盖 -interval 后重新派生 tick 数（公开入口）。
func (c *Config) DeriveForInterval() { c.derive() }

// LoadConfig 装载配置：内置默认 ← <数据目录>/config.json ← <数据目录>/agents.json。
// 文件不存在时写入模板；文件存在但解析失败时返回告警（继续用默认值运行——
// 诊断工具不能因为配置写错就哑掉，但必须让用户知道配置没生效）。
func LoadConfig(dataDir string) (Config, []string) {
	cfg := DefaultConfig()
	var warns []string
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	_ = os.MkdirAll(cfg.DataDir, 0o755)

	type fileCfg struct {
		IntervalMs    int                `json:"intervalMs"`
		ProcEverySec  int                `json:"procEverySec"`
		RetentionDays int                `json:"retentionDays"`
		ETW           *bool              `json:"etwIO"`
		Thresholds    *health.Thresholds `json:"thresholds"`
	}
	cfgPath := filepath.Join(cfg.DataDir, "config.json")
	b, err := os.ReadFile(cfgPath)
	switch {
	case err == nil:
		var fc fileCfg
		if jerr := json.Unmarshal(b, &fc); jerr != nil {
			warns = append(warns, fmt.Sprintf("config.json 解析失败，已用默认值: %v", jerr))
		} else {
			if fc.IntervalMs >= 250 {
				cfg.Interval = time.Duration(fc.IntervalMs) * time.Millisecond
			}
			if fc.ProcEverySec >= 1 {
				cfg.ProcEverySec = fc.ProcEverySec
			}
			if fc.RetentionDays >= 1 {
				cfg.RetentionDays = fc.RetentionDays
			}
			if fc.ETW != nil {
				cfg.ETW = *fc.ETW
			}
			if fc.Thresholds != nil {
				cfg.Thresholds = mergeThresholds(cfg.Thresholds, *fc.Thresholds)
			}
		}
	case os.IsNotExist(err):
		// 模板带全默认值（thresholds 不为 null），用户改哪个都有效。
		th := health.DefaultThresholds()
		etw := true
		tpl := fileCfg{IntervalMs: 2000, ProcEverySec: ProcSliceSec, RetentionDays: 14, ETW: &etw, Thresholds: &th}
		if b2, e := json.MarshalIndent(tpl, "", "  "); e == nil {
			_ = os.WriteFile(cfgPath, b2, 0o644)
		}
	default:
		warns = append(warns, fmt.Sprintf("config.json 读取失败，已用默认值: %v", err))
	}

	agentsPath := filepath.Join(cfg.DataDir, "agents.json")
	user, aerr := attrib.LoadUserPatterns(agentsPath)
	switch {
	case aerr == nil && len(user) > 0:
		cfg.Patterns = append(cfg.Patterns, user...)
	case os.IsNotExist(aerr):
		_ = os.WriteFile(agentsPath, []byte("{\n  \"agents\": []\n}\n"), 0o644)
	case aerr != nil:
		warns = append(warns, fmt.Sprintf("agents.json 解析失败，已用内置规则: %v", aerr))
	}
	cfg.derive()
	return cfg, warns
}

// mergeThresholds 用文件中的非零字段覆盖默认值。
func mergeThresholds(def, over health.Thresholds) health.Thresholds {
	set := func(dst *float64, v float64) {
		if v > 0 {
			*dst = v
		}
	}
	set(&def.CPUDegraded, over.CPUDegraded)
	set(&def.CPUSaturated, over.CPUSaturated)
	set(&def.MemDegraded, over.MemDegraded)
	set(&def.MemSaturated, over.MemSaturated)
	set(&def.CommitDegraded, over.CommitDegraded)
	set(&def.CommitSaturated, over.CommitSaturated)
	set(&def.DiskBusyDegraded, over.DiskBusyDegraded)
	set(&def.DiskBusySaturated, over.DiskBusySaturated)
	set(&def.DiskLatDegradedMs, over.DiskLatDegradedMs)
	set(&def.DiskLatSaturatedMs, over.DiskLatSaturatedMs)
	set(&def.GPUUtilDegraded, over.GPUUtilDegraded)
	set(&def.GPUUtilSaturated, over.GPUUtilSaturated)
	set(&def.VRAMDegraded, over.VRAMDegraded)
	set(&def.VRAMSaturated, over.VRAMSaturated)
	return def
}
