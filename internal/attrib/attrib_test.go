package attrib

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lomehong/reshmon/internal/model"
)

func TestTreeAttribution(t *testing.T) {
	e := New([]Pattern{{Match: "myagent", Label: "MyAgent"}})
	procs := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "myagent.exe"},
		{PID: 2, PPID: 1, Name: "cmd.exe"},
		{PID: 3, PPID: 2, Name: "go.exe"}, // 孙进程也要归入 Agent
		{PID: 4, PPID: 0, Name: "explorer.exe"},
		{PID: 5, PPID: 4, Name: "chrome.exe"},
	}
	e.Attribute(procs)
	if procs[0].Agent != "MyAgent" || procs[1].Agent != "MyAgent" || procs[2].Agent != "MyAgent" {
		t.Fatalf("Agent 树归因失败: %q %q %q", procs[0].Agent, procs[1].Agent, procs[2].Agent)
	}
	if procs[3].Agent != "explorer.exe" || procs[4].Agent != "explorer.exe" {
		t.Fatalf("根名桶归因失败: %q %q", procs[3].Agent, procs[4].Agent)
	}
}

func TestStickyAfterParentDeath(t *testing.T) {
	e := New([]Pattern{{Match: "myagent", Label: "MyAgent"}})
	first := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "myagent.exe"},
		{PID: 2, PPID: 1, Name: "compiler.exe"},
	}
	e.Attribute(first)
	if first[1].Agent != "MyAgent" {
		t.Fatalf("预置失败: %q", first[1].Agent)
	}
	// 父进程死亡后，孤儿仍保持原归属
	second := []model.ProcStat{{PID: 2, PPID: 1, Name: "compiler.exe"}}
	e.Attribute(second)
	if second[0].Agent != "MyAgent" {
		t.Fatalf("sticky 归因失败: %q", second[0].Agent)
	}
}

func TestPathPattern(t *testing.T) {
	e := New([]Pattern{{Match: "path:D:\\AI\\myagent", Label: "PathAgent"}})
	procs := []model.ProcStat{{PID: 9, PPID: 0, Name: "node.exe", Path: `D:\AI\myagent\server\node.exe`}}
	e.Attribute(procs)
	if procs[0].Agent != "PathAgent" {
		t.Fatalf("path: 规则失败: %q", procs[0].Agent)
	}
}

func TestSysRootBuckets(t *testing.T) {
	e := New(nil)
	procs := []model.ProcStat{
		{PID: 100, PPID: 0, Name: "services.exe"},
		{PID: 101, PPID: 100, Name: "svchost.exe"},
	}
	e.Attribute(procs)
	if procs[1].Agent != "系统服务" {
		t.Fatalf("系统服务桶失败: %q", procs[1].Agent)
	}
}

func TestAggregate(t *testing.T) {
	procs := []model.ProcStat{
		{Agent: "A", CpuPct: 10, MemPrivB: 100, ReadBps: 5, GpuPct: 60},
		{Agent: "A", CpuPct: 5, MemPrivB: 50, WriteBps: 5, GpuPct: 80},
		{Agent: "B", CpuPct: 1, MemPrivB: 7},
	}
	out := Aggregate(procs)
	if len(out) != 2 {
		t.Fatalf("桶数错误: %d", len(out))
	}
	if out[0].Name != "A" || out[0].CpuPct != 15 || out[0].MemPrivB != 150 {
		t.Fatalf("A 聚合错误: %+v", out[0])
	}
	if out[0].ReadBps != 5 || out[0].WriteBps != 5 {
		t.Fatalf("A IO 聚合错误: %+v", out[0])
	}
	if out[0].GpuPct != 140 {
		t.Fatalf("GPU 聚合口径错误（进程级封顶 100 后求和）: %+v", out[0])
	}
	if out[1].Procs != 1 {
		t.Fatalf("进程计数错误")
	}
}

func TestLoadUserPatternsMissing(t *testing.T) {
	p, err := LoadUserPatterns(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || p != nil {
		t.Fatalf("缺失文件应返回 nil,nil: %v %v", p, err)
	}
}

func TestLoadUserPatterns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(path, []byte(`{"agents":[{"match":"foo","label":"Foo"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadUserPatterns(path)
	if err != nil || len(p) != 1 || p[0].Label != "Foo" {
		t.Fatalf("读取失败: %v %v", p, err)
	}
}
