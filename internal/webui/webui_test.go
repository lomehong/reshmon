//go:build windows

package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/app"
)

func newTestApp(t *testing.T) *app.App {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Interval = 500 * time.Millisecond // 亚秒间隔：同时验证 /api/history 不再除零
	cfg.Record = false
	cfg.DeriveForInterval()
	a, err := app.Start(cfg)
	if err != nil {
		t.Skipf("采样器不可用: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// 回归：interval<1s 时 /api/history 的整型除法除零 panic（趋势图全挂）。
func TestHistorySubSecondInterval(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(Handler(a))
	defer srv.Close()
	// 等首拍就绪，避免 starting 态干扰
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := a.Current(); s != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp, err := srv.Client().Get(srv.URL + "/api/history?sec=300")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/history 应 200: %d", resp.StatusCode)
	}
}

// Host 白名单（DNS rebinding 防御）：绑定回环时只接受本机 Host 头。
func TestHostGuard(t *testing.T) {
	a := newTestApp(t)
	allowed := map[string]bool{"127.0.0.1:8964": true, "localhost:8964": true}
	h := handler(a, allowed)
	ok := httptest.NewRequest("GET", "http://127.0.0.1:8964/api/snapshot", nil)
	ok.Host = "127.0.0.1:8964"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, ok)
	if w.Code != http.StatusOK {
		t.Fatalf("本机 Host 应放行: %d", w.Code)
	}
	evil := httptest.NewRequest("GET", "http://127.0.0.1:8964/api/snapshot", nil)
	evil.Host = "evil.example.com:8964" // DNS 重绑后的 Host
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, evil)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("陌生 Host 应 403: %d", w2.Code)
	}
	// 通配绑定（allowedHosts==nil）不做校验——用户主动暴露的场景
	h2 := handler(a, nil)
	w3 := httptest.NewRecorder()
	h2.ServeHTTP(w3, evil)
	if w3.Code == http.StatusForbidden {
		t.Fatal("通配绑定不应拦截")
	}
}

// MCP Streamable HTTP 端点：POST /mcp 走 MCP 协议，GET 拒 405，
// 纯通知批次答 202；与 Host 白名单共存。
func TestMCPHTTPEndpoint(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(Handler(a))
	defer srv.Close()
	client := srv.Client()

	// initialize + tools/call（Streamable HTTP 单对象 POST）
	initResp, err := client.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer initResp.Body.Close()
	var init map[string]any
	if err := json.NewDecoder(initResp.Body).Decode(&init); err != nil {
		t.Fatal(err)
	}
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "reshmon" {
		t.Fatalf("initialize 响应错误: %v", init)
	}

	// 等首拍就绪（500ms 间隔的嵌入式采样）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := a.Current(); s != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	callResp, err := client.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"top_processes","arguments":{"dimension":"cpu","limit":3}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer callResp.Body.Close()
	var call map[string]any
	if err := json.NewDecoder(callResp.Body).Decode(&call); err != nil {
		t.Fatal(err)
	}
	content := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "pid") {
		t.Fatalf("tools/call 应返回进程数据: %v", content)
	}

	// GET → 405（无服务器推送流，规范允许）
	getResp, err := client.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp 应 405: %d", getResp.StatusCode)
	}

	// 纯通知 → 202 无 body
	notiResp, err := client.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	notiResp.Body.Close()
	if notiResp.StatusCode != http.StatusAccepted {
		t.Fatalf("纯通知应 202: %d", notiResp.StatusCode)
	}
}

// /api/incidents 走内存（含 -no-record 会话事件），不再每次轮询全量读盘。
func TestIncidentsFromMemory(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(Handler(a))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/api/incidents?n=50")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"incidents"`)) {
		t.Fatalf("响应缺少 incidents 字段: %s", body)
	}
}
