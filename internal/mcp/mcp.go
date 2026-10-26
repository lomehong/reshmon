//go:build windows

// Package mcp 实现 Model Context Protocol（stdio 传输）：把 reshmon 的
// 资源视野以 MCP tools 暴露给 AI Agent——Agent 在决定自己开多大并发
// 之前，先看一眼机器现在是谁在吃资源。
//
// 协议帧：按行分隔的 JSON-RPC 2.0（stdout 为协议通道，日志一律走 stderr）。
// 运行时自包含：内嵌只读采样（不落盘、不抢数据目录写锁），可多个 Agent
// 各自启动互不干扰。
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

// Source MCP 工具的数据源（*app.App 天然满足；测试可注入伪源）。
type Source interface {
	Current() (*model.Snapshot, health.Report)
	Incidents() []store.Incident
	Errs() []string
	Ring() *store.Ring
	Interval() time.Duration
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// Serve 在 in/out 上运行 MCP 协议直到 EOF（阻塞）。errW 用于诊断日志。
// 工具均为无状态只读，无需跨请求会话状态。
func Serve(src Source, version string, in io.Reader, out, errW io.Writer) error {
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		resp, hasResp := handleLine(src, version, []byte(line))
		if !hasResp {
			continue // notification：不回包
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// HandleBody 处理一次 HTTP POST 的 MCP 请求体（Streamable HTTP 传输）：
// 支持单对象与 JSON-RPC 批量数组；全为 notification 时 ok=false（HTTP
// 层应答 202 无 body）。
func HandleBody(src Source, version string, body []byte) (resp any, ok bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return parseError(err), true
		}
		out := make([]rpcResponse, 0, len(batch))
		for _, item := range batch {
			if r, has := handleLine(src, version, item); has {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil, false // 纯通知批次：无响应（HTTP 层答 202）
		}
		return out, true
	}
	r, has := handleLine(src, version, trimmed)
	return r, has
}

func handleLine(src Source, version string, line []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &jsonRPCError{Code: -32700, Message: "解析错误: " + err.Error()}}, true
	}
	if len(req.ID) == 0 { // notification：不回包
		return rpcResponse{}, false
	}
	return dispatch(src, version, &req), true
}

func parseError(err error) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
		Error: &jsonRPCError{Code: -32700, Message: "解析错误: " + err.Error()}}
}

func dispatch(src Source, version string, req *rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "reshmon", "version": version},
		}}
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolDefs()}}
	case "tools/call":
		return callTool(src, req.ID, req.Params)
	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCError{Code: -32601, Message: "未知方法: " + req.Method}}
	}
}

func toolDefs() []map[string]any {
	schema := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []map[string]any{
		{
			"name":        "resource_snapshot",
			"description": "当前资源健康快照：CPU/内存/提交/每块磁盘/每块 GPU/网络 + 健康结论（ok/warn/sat）与降级说明。决定并发前先看这个。",
			"inputSchema": schema(map[string]any{
				"includeProcesses": map[string]any{"type": "boolean", "description": "附带进程 Top（默认 false）"},
			}),
		},
		{
			"name":        "agent_usage",
			"description": "按 Agent/系统桶聚合的资源占用（CPU%、私有内存、磁盘 IO、GPU%、进程数）。回答\"现在哪些 Agent 在吃资源、各吃多少\"。",
			"inputSchema": schema(map[string]any{}),
		},
		{
			"name":        "top_processes",
			"description": "按维度取进程 Top：cpu/mem/io/gpu。",
			"inputSchema": schema(map[string]any{
				"dimension": map[string]any{"type": "string", "enum": []string{"cpu", "mem", "io", "gpu"}, "description": "排序维度（默认 cpu）"},
				"limit":     map[string]any{"type": "integer", "description": "返回条数，默认 15，上限 50"},
			}),
		},
		{
			"name":        "resource_trend",
			"description": "最近 N 分钟（1-120）各维度趋势摘要：均值与峰值。判断\"是在变好还是变坏\"。",
			"inputSchema": schema(map[string]any{
				"minutes": map[string]any{"type": "integer", "description": "回看时长（分钟），默认 10"},
			}),
		},
		{
			"name":        "health_events",
			"description": "最近健康状态迁移事件（如 磁盘 C: ok→sat），判断异常何时开始。",
			"inputSchema": schema(map[string]any{
				"limit": map[string]any{"type": "integer", "description": "返回条数，默认 20，上限 100"},
			}),
		},
	}
}

func callTool(src Source, id json.RawMessage, params json.RawMessage) rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	var result any
	var err error
	switch p.Name {
	case "resource_snapshot":
		result, err = toolSnapshot(src, p.Arguments)
	case "agent_usage":
		result, err = toolAgentUsage(src)
	case "top_processes":
		result, err = toolTopProcesses(src, p.Arguments)
	case "resource_trend":
		result, err = toolTrend(src, p.Arguments)
	case "health_events":
		result, err = toolEvents(src, p.Arguments)
	default:
		return toolError(id, fmt.Sprintf("未知工具 %q", p.Name))
	}
	if err != nil {
		return toolError(id, err.Error())
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
		"content": []any{map[string]any{"type": "text", "text": mustJSON(result)}},
		"isError": false,
	}}
}

func toolError(id json.RawMessage, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"` + err.Error() + `"}`
	}
	return string(b)
}
