//go:build windows

package webui

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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
