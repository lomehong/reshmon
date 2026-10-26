//go:build windows

// Package webui 提供 reshmon 的 Web 面板：嵌入式单页 + JSON API。
// 面板读环形缓冲（最近约 2 小时），长历史回溯用 reshmon report。
package webui

import (
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"time"

	"github.com/lomehong/reshmon/internal/app"
	"github.com/lomehong/reshmon/internal/mcp"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
	"github.com/lomehong/reshmon/internal/syswin"
)

//go:embed index.html
var indexHTML []byte

// Serve 启动面板（阻塞，Ctrl+C 优雅退出）。
func Serve(a *app.App, addr string) error {
	srv := &http.Server{Addr: addr, Handler: HandlerWithAddr(a, addr), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-sig:
		// 优雅退出：让 main 的 defer 链（App.Close 释放句柄与目录锁）执行
		_ = srv.Close()
		return nil
	}
}

// Handler 面板路由（不做 Host 校验，测试用）。
func Handler(a *app.App) http.Handler { return handler(a, nil) }

// HandlerWithAddr 按监听地址构造路由；绑定回环地址时启用 Host 白名单
// （防 DNS rebinding：恶意网页把域名重绑到 127.0.0.1 后借浏览器同源
// 读走全量进程表）。绑定 0.0.0.0 等通配地址属用户主动暴露，不做校验，
// 风险见 README。
func HandlerWithAddr(a *app.App, addr string) http.Handler {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return handler(a, nil)
	}
	allowed := map[string]bool{
		addr: true, host + ":" + port: true,
		"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true,
	}
	return handler(a, allowed)
}

func handler(a *app.App, allowedHosts map[string]bool) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snap, rep := a.Current()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if snap == nil {
			writeJSON(w, map[string]any{"starting": true})
			return
		}
		procs := store.TrimProcs(snap.Procs, 80)
		sort.Slice(procs, func(i, j int) bool { return procs[i].CpuPct > procs[j].CpuPct })
		host, _ := os.Hostname()
		resp := map[string]any{
			"ts":         snap.TS.Format(time.RFC3339),
			"intervalMs": snap.Interval.Milliseconds(),
			"sys": map[string]any{
				"host":      host,
				"uptimeSec": syswin.UptimeMs() / 1000,
				"cores":     snap.CPU.Cores,
				"version":   app.Version,
				"dataDir":   a.Cfg.DataDir,
				"record":    a.Cfg.Record,
			},
			"health":    rep,
			"cpu":       snap.CPU,
			"mem":       snap.Mem,
			"disks":     snap.Disks,
			"gpus":      snap.GPUs,
			"nics":      snap.NICs,
			"agents":    snap.Agents,
			"procs":     procs,
			"capErrors": snap.CapErrors,
			"errors":    a.Errs(),
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		sec := 300
		if v, err := strconv.Atoi(r.URL.Query().Get("sec")); err == nil && v > 0 && v <= 7200 {
			sec = v
		}
		cutoff := time.Now().Add(-time.Duration(sec) * time.Second).UnixMilli()
		// 按真实间隔换算（浮点）：interval<1s 时整型除法会除零 panic
		est := int(float64(sec)/a.Cfg.Interval.Seconds()) + 8
		if est < 8 {
			est = 8
		}
		ticks := a.Ring().Last(est)
		out := make([]model.Totals, 0, len(ticks))
		for _, t := range ticks {
			if t.T >= cutoff {
				out = append(out, t)
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeJSON(w, map[string]any{"ticks": out})
	})

	mux.HandleFunc("/api/procs", func(w http.ResponseWriter, r *http.Request) {
		snap, _ := a.Current()
		if snap == nil {
			writeJSON(w, map[string]any{"procs": []any{}})
			return
		}
		procs := append([]model.ProcStat(nil), snap.Procs...)
		sort.Slice(procs, func(i, j int) bool { return procs[i].CpuPct > procs[j].CpuPct })
		writeJSON(w, map[string]any{"ts": snap.TS.Format(time.RFC3339), "procs": procs})
	})

	mux.HandleFunc("/api/incidents", func(w http.ResponseWriter, r *http.Request) {
		n := 50
		if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 500 {
			n = v
		}
		// 全走内存（启动时已回放历史文件）：不再每个轮询周期全量读盘解析
		cutoff := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
		all := a.Incidents()
		incs := make([]store.Incident, 0, n)
		for _, inc := range all {
			if inc.T >= cutoff {
				incs = append(incs, inc)
			}
		}
		if len(incs) > n {
			incs = incs[len(incs)-n:]
		}
		writeJSON(w, map[string]any{"incidents": incs})
	})

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// MCP Streamable HTTP：serve 已在跑时 Agent 走这里（单实例采样 +
		// ETW 会话，多 Agent 共享同一份口径）；stdio 版 reshmon mcp 是
		// serve 未运行时的独立兜底。工具均无状态，不分配会话 ID：
		// GET/DELETE 返回 405（规范允许），纯通知批次答 202。
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp, ok := mcp.HandleBody(a, app.Version, body)
		if !ok {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, resp)
	})

	if allowedHosts != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowedHosts[r.Host] {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
