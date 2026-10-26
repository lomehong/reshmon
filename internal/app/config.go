// Package app 常驻运行时：定时采样 → 环形缓冲/落盘 → 健康判定 → 事件记录，
// 并承载配置装载。live/serve/once 共用。
package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/lomehong/reshmon/internal/attrib"
	"github.com/lomehong/reshmon/internal/health"
)

type Config struct {
	DataDir       string
	Interval      time.Duration
	ProcEvery     int // 每 N tick 落一次全量进程切片
	RetentionDays int
	RingCap       int
	Thresholds    health.Thresholds
	Patterns      []attrib.Pattern
	Record        bool
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
		ProcEvery:     15,
		RetentionDays: 14,
		RingCap:       3600,
		Thresholds:    health.DefaultThresholds(),
		Patterns:      attrib.DefaultPatterns(),
		Record:        true,
	}
}

// LoadConfig 装载配置：内置默认 ← <数据目录>/config.json ← <数据目录>/agents.json。
// 两个文件不存在时自动写入模板，便于用户直接改。
func LoadConfig(dataDir string) Config {
	cfg := DefaultConfig()
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	_ = os.MkdirAll(cfg.DataDir, 0o755)

	type fileCfg struct {
		IntervalMs    int                `json:"intervalMs"`
		ProcEvery     int                `json:"procEvery"`
		RetentionDays int                `json:"retentionDays"`
		Thresholds    *health.Thresholds `json:"thresholds"`
	}
	cfgPath := filepath.Join(cfg.DataDir, "config.json")
	if b, err := os.ReadFile(cfgPath); err == nil {
		var fc fileCfg
		if json.Unmarshal(b, &fc) == nil {
			if fc.IntervalMs >= 250 {
				cfg.Interval = time.Duration(fc.IntervalMs) * time.Millisecond
			}
			if fc.ProcEvery >= 1 {
				cfg.ProcEvery = fc.ProcEvery
			}
			if fc.RetentionDays >= 1 {
				cfg.RetentionDays = fc.RetentionDays
			}
			if fc.Thresholds != nil {
				cfg.Thresholds = mergeThresholds(cfg.Thresholds, *fc.Thresholds)
			}
		}
	} else if os.IsNotExist(err) {
		tpl := fileCfg{IntervalMs: 2000, ProcEvery: 15, RetentionDays: 14}
		if b2, e := json.MarshalIndent(tpl, "", "  "); e == nil {
			_ = os.WriteFile(cfgPath, b2, 0o644)
		}
	}

	agentsPath := filepath.Join(cfg.DataDir, "agents.json")
	if user, err := attrib.LoadUserPatterns(agentsPath); err == nil && len(user) > 0 {
		cfg.Patterns = append(cfg.Patterns, user...)
	} else if os.IsNotExist(err) {
		_ = os.WriteFile(agentsPath, []byte("{\n  \"agents\": []\n}\n"), 0o644)
	}
	return cfg
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
