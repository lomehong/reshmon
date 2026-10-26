//go:build windows

package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

type fakeSource struct {
	snap *model.Snapshot
	rep  health.Report
	incs []store.Incident
	errs []string
	ring *store.Ring
}

func (f *fakeSource) Current() (*model.Snapshot, health.Report) { return f.snap, f.rep }
func (f *fakeSource) Incidents() []store.Incident               { return f.incs }
func (f *fakeSource) Errs() []string                            { return f.errs }
func (f *fakeSource) Ring() *store.Ring                         { return f.ring }
func (f *fakeSource) Interval() time.Duration                   { return 2 * time.Second }

func newFake() *fakeSource {
	r := store.NewRing(3600)
	now := time.Now()
	for i := 0; i < 30; i++ {
		r.Push(model.Totals{T: now.Add(-time.Duration(30-i) * 2 * time.Second).UnixMilli(), CPU: 50, MemUsedPct: 60})
	}
	return &fakeSource{
		snap: &model.Snapshot{
			TS: now, Interval: 2 * time.Second,
			CPU: model.CPUStat{BusyPct: 42.5, Cores: 16},
			Mem: model.MemStat{PhysUsedPct: 61.2, PhysAvailB: 1 << 30},
			Procs: []model.ProcStat{
				{PID: 1, Name: "zcode.exe", Agent: "ZCode", CpuPct: 12.5, MemPrivB: 2 << 30},
				{PID: 2, Name: "go.exe", Agent: "ZCode", CpuPct: 8, ReadBps: 1 << 20},
			},
			Agents: []model.AgentStat{{Name: "ZCode", CpuPct: 20.5, MemPrivB: 2 << 30, Procs: 2}},
			Disks:  []model.DiskStat{{ID: "0 C:", Letters: "C:", BusyPct: 3.2}},
			NICs:   []model.NICStat{{Name: "eth0", RecvBps: 100, SentBps: 50}},
		},
		rep:  health.Report{Overall: model.StatusOK},
		ring: r,
		incs: []store.Incident{{T: now.Add(-time.Minute).UnixMilli(), Dim: "cpu", From: "ok", To: "warn"}},
	}
}

func runRPC(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	src := newFake()
	var in bytes.Buffer
	for _, l := range lines {
		in.WriteString(l + "\n")
	}
	var out bytes.Buffer
	if err := Serve(src, "test-0.0.1", &in, &out, &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("响应不是合法 JSON: %q %v", l, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func TestHandshakeAndToolsList(t *testing.T) {
	resps := runRPC(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	if len(resps) != 3 { // notification 不回包
		t.Fatalf("应答 3 条（通知不回包），得到 %d: %v", len(resps), resps)
	}
	init := resps[0]
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "reshmon" {
		t.Fatalf("initialize 响应错误: %v", init)
	}
	if init["id"].(float64) != 1 {
		t.Fatalf("请求 ID 必须回带: %v", init)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("应有 5 个工具: %d", len(tools))
	}
	if resps[2]["result"].(map[string]any) == nil {
		t.Fatal("ping 应返回空 result")
	}
}

func TestToolsCall(t *testing.T) {
	resps := runRPC(t,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"agent_usage","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"resource_trend","arguments":{"minutes":1}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"top_processes","arguments":{"dimension":"io","limit":5}}}`,
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if len(resps) != 4 {
		t.Fatalf("应答 4 条: %d", len(resps))
	}
	textOf := func(i int) string {
		res := resps[i]["result"].(map[string]any)
		content := res["content"].([]any)[0].(map[string]any)
		return content["text"].(string)
	}
	var usage struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal([]byte(textOf(0)), &usage); err != nil || len(usage.Agents) != 1 || usage.Agents[0]["agent"] != "ZCode" {
		t.Fatalf("agent_usage 输出错误: %s %v", textOf(0), err)
	}
	var trend struct {
		Samples int `json:"samples"`
		CPU     struct {
			Avg float64 `json:"avg"`
		} `json:"cpu"`
	}
	if err := json.Unmarshal([]byte(textOf(1)), &trend); err != nil || trend.Samples == 0 || trend.CPU.Avg != 50 {
		t.Fatalf("resource_trend 输出错误: %s %v", textOf(1), err)
	}
	if !strings.Contains(textOf(2), "go.exe") {
		t.Fatalf("top_processes(io) 应含 go.exe: %s", textOf(2))
	}
	// 未知工具 → isError:true 的工具结果（协议正确形态），且 ID 回带
	res13 := resps[3]["result"].(map[string]any)
	if res13["isError"] != true {
		t.Fatalf("未知工具应 isError: %v", res13)
	}
	if resps[3]["id"].(float64) != 13 {
		t.Fatalf("ID 未回带: %v", resps[3])
	}
}

func TestBadJSONAndUnknownMethod(t *testing.T) {
	resps := runRPC(t,
		`{bad json`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/subscribe"}`,
	)
	if resps[0]["error"] == nil {
		t.Fatal("坏 JSON 应返回 -32700")
	}
	if code := resps[1]["error"].(map[string]any)["code"].(float64); code != -32601 {
		t.Fatalf("未知方法应 -32601: %v", resps[1])
	}
}
