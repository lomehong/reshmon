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

// 嵌套 Agent 各记各的：ZCode 里跑 Claude——最近的 Agent 祖先优先，
// Claude 及其子树归 Claude，ZCode 自身与其余子树归 ZCode。
func TestNestedAgentsNearestFirst(t *testing.T) {
	e := New([]Pattern{
		{Match: "zcode", Label: "ZCode"},
		{Match: "claude", Label: "Claude"},
	})
	procs := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "zcode.exe"},
		{PID: 2, PPID: 1, Name: "claude.exe"},
		{PID: 3, PPID: 2, Name: "node.exe"}, // Claude 的子进程 → Claude
		{PID: 4, PPID: 1, Name: "go.exe"},   // ZCode 的其余子进程 → ZCode
	}
	e.Attribute(procs)
	if procs[0].Agent != "ZCode" {
		t.Fatalf("zcode 自身归因错误: %q", procs[0].Agent)
	}
	if procs[1].Agent != "Claude" || procs[2].Agent != "Claude" {
		t.Fatalf("嵌套内层 Agent 归因错误: %q %q", procs[1].Agent, procs[2].Agent)
	}
	if procs[3].Agent != "ZCode" {
		t.Fatalf("外层其余子树归因错误: %q", procs[3].Agent)
	}
}

// PID 复用防护：同一 PID 换了进程（创建时间不同）后，陈旧 sticky 立即作废，
// 不把新进程记到前任的 Agent 头上。
func TestStickyPIDReuseInvalidated(t *testing.T) {
	e := New([]Pattern{{Match: "myagent", Label: "MyAgent"}})
	first := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "myagent.exe", Start: 100},
		{PID: 42, PPID: 1, Name: "compiler.exe", Start: 200},
	}
	e.Attribute(first)
	if first[1].Agent != "MyAgent" {
		t.Fatalf("预置失败: %q", first[1].Agent)
	}
	// PID 42 被新进程复用（Start 不同）且是孤儿：sticky 不再生效
	second := []model.ProcStat{{PID: 42, PPID: 999, Name: "svchost.exe", Start: 5000}}
	e.Attribute(second)
	if second[0].Agent == "MyAgent" {
		t.Fatalf("PID 复用后不应沿用 sticky: %q", second[0].Agent)
	}
	if second[0].Agent != "svchost.exe" {
		t.Fatalf("应回落根名桶: %q", second[0].Agent)
	}
}

// 系统桶优先于 sticky：链上可见的系统根不被陈旧 sticky 抢占。
func TestSysRootBeatsSticky(t *testing.T) {
	e := New([]Pattern{{Match: "myagent", Label: "MyAgent"}})
	first := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "myagent.exe"},
		{PID: 42, PPID: 1, Name: "compiler.exe"},
	}
	e.Attribute(first)
	// PID 42 被复用为服务进程且父链指向活的 services.exe
	second := []model.ProcStat{
		{PID: 7, PPID: 0, Name: "services.exe"},
		{PID: 42, PPID: 7, Name: "svchost.exe"},
	}
	e.Attribute(second)
	if second[1].Agent != "系统服务" {
		t.Fatalf("系统桶应优先于 sticky: %q", second[1].Agent)
	}
}

// name: 前缀精确匹配——"droid" 子串会误伤一切含 android 的进程名。
func TestNamePattern(t *testing.T) {
	e := New([]Pattern{{Match: "name:droid.exe", Label: "Droid"}})
	procs := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "android.exe"},
		{PID: 2, PPID: 0, Name: "droid.exe"},
	}
	e.Attribute(procs)
	if procs[0].Agent == "Droid" {
		t.Fatal("android.exe 不应命中 droid 规则")
	}
	if procs[1].Agent != "Droid" {
		t.Fatalf("droid.exe 应命中: %q", procs[1].Agent)
	}
}

// 假父剔除：PPID 指向一个比"子进程"创建得更晚的进程（PID 复用产物），
// 链应在此截断，不把子进程归到假父的树下。
func TestFakeParentPruned(t *testing.T) {
	e := New([]Pattern{{Match: "myagent", Label: "MyAgent"}})
	procs := []model.ProcStat{
		{PID: 1, PPID: 0, Name: "explorer.exe", Start: 1000},
		{PID: 2, PPID: 1, Name: "worker.exe", Start: 900}, // "父"比子晚创建 → 假父
		{PID: 3, PPID: 0, Name: "myagent.exe", Start: 100},
	}
	e.Attribute(procs)
	if procs[1].Agent == "MyAgent" {
		t.Fatal("假父链不应把进程归给无关 Agent")
	}
}
