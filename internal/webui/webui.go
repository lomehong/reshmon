//go:build windows

// Package webui 提供 reshmon 的 Web 面板：嵌入式单页 + JSON API。
// 面板读环形缓冲（最近约 2 小时），长历史回溯用 reshmon report。
package webui

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
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
func Serve(a *app.App, addr string) error { return ServeUntil(a, addr, nil) }

// ServeUntil 阻塞至 Ctrl+C 或 stop 通道关闭（服务模式传 SCM 停止信号）。
// 返回前已优雅关闭 HTTP 服务，调用方随后自行释放 App。
func ServeUntil(a *app.App, addr string, stop <-chan struct{}) error {
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
	case <-stop:
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
		// 升级二进制后面板必须跟着换：禁止浏览器缓存旧版嵌入页面
		// （实测过缓存旧 HTML 导致新功能不显示的反馈）。
		w.Header().Set("Cache-Control", "no-cache")
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

	// 回溯某一时刻的进程概览（面板点击趋势图）：t 为 unix 毫秒。
	// 落在数据缺失区间时返回 blind + 原因，不返回空列表。
	mux.HandleFunc("/api/at", func(w http.ResponseWriter, r *http.Request) {
		ms, err := strconv.ParseInt(r.URL.Query().Get("t"), 10, 64)
		if err != nil || ms <= 0 {
			http.Error(w, "需要 t=<unix 毫秒>", http.StatusBadRequest)
			return
		}
		writeJSON(w, a.At(time.UnixMilli(ms)))
	})

	// 端口→进程归属：按需枚举（不进周期采样），支持按端口/PID 过滤。
	mux.HandleFunc("/api/conns", func(w http.ResponseWriter, r *http.Request) {
		conns, err := a.Conns()
		if err != nil {
			http.Error(w, "枚举网络端点失败："+err.Error(), http.StatusInternalServerError)
			return
		}
		// 附进程名与 Agent 归属（取自最近一拍快照）
		owners := map[uint32]connOwner{}
		if snap, _ := a.Current(); snap != nil {
			for _, p := range snap.Procs {
				if _, ok := owners[p.PID]; !ok {
					owners[p.PID] = connOwner{proc: p.Name, agent: p.Agent}
				}
			}
		}
		q := r.URL.Query()
		rows := buildConnRows(conns, owners, q.Get("port"), q.Get("pid"))
		writeJSON(w, map[string]any{"conns": rows, "total": len(conns)})
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

// connOwner 一个 PID 对应的归属信息（进程名 + Agent 桶）。
type connOwner struct{ proc, agent string }

// connRow 一条端点及其归属（/api/conns 的输出行）。
type connRow struct {
	Proto  string `json:"proto"`
	Local  string `json:"local"`
	Remote string `json:"remote,omitempty"`
	State  string `json:"state,omitempty"`
	PID    uint32 `json:"pid"`
	Proc   string `json:"proc,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// buildConnRows 过滤、附归属并排序端点。port/pid 为空表示不过滤。
func buildConnRows(conns []model.ConnStat, owners map[uint32]connOwner, port, pid string) []connRow {
	rows := make([]connRow, 0, len(conns))
	for _, c := range conns {
		if pid != "" && strconv.FormatUint(uint64(c.PID), 10) != pid {
			continue
		}
		if port != "" && !connPortMatches(c, port) {
			continue
		}
		o := owners[c.PID]
		rows = append(rows, connRow{
			Proto: c.Proto, Local: c.Local, Remote: c.Remote, State: c.State,
			PID: c.PID, Proc: o.proc, Agent: o.agent,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return connCompare(rows[i], rows[j]) < 0 })
	return rows
}

// connPortMatches 按端口过滤端点：匹配本地端口或对端端口。
func connPortMatches(c model.ConnStat, port string) bool {
	for _, addr := range []string{c.Local, c.Remote} {
		if addr == "" {
			continue
		}
		if _, p, err := net.SplitHostPort(addr); err == nil && p == port {
			return true
		}
	}
	return false
}

// connGroup 端点分组（排序的第一优先级）：
//
//	0 = 可接入的端点（TCP 监听 / UDP 绑定）——"谁占着这个端口"找的就是这些
//	1 = 其余连接（已建立、等待关闭等）
//	2 = 本地端口为 0 的端点——系统临时套接字（发 DNS 等一次性用途），
//	    按端口查不到它们，按数值排序又会全部挤到最前面（实测一台机器
//	    上有 38 条），所以单独放最后。
func connGroup(c connRow) int {
	if _, p := splitEndpoint(c.Local); p == 0 {
		return 2
	}
	if c.State == "LISTEN" || strings.HasPrefix(c.Proto, "UDP") {
		return 0
	}
	return 1
}

// connCompare 端点排序依据：可接入的端点在前；两组内部都按**本地端口数值**
// 升序——端口是这张表的主键，按地址字符串排序会把 445 排到 15100 之后。
// 同端口再依次按协议、本地 IP、对端、PID。返回负/零/正。
func connCompare(a, b connRow) int {
	if ga, gb := connGroup(a), connGroup(b); ga != gb {
		if ga < gb {
			return -1
		}
		return 1
	}
	aip, aport := splitEndpoint(a.Local)
	bip, bport := splitEndpoint(b.Local)
	if aport != bport {
		if aport < bport {
			return -1
		}
		return 1
	}
	if a.Proto != b.Proto {
		return strings.Compare(a.Proto, b.Proto)
	}
	if c := compareIP(aip, bip); c != 0 {
		return c
	}
	if a.Remote != b.Remote {
		return strings.Compare(a.Remote, b.Remote)
	}
	switch {
	case a.PID < b.PID:
		return -1
	case a.PID > b.PID:
		return 1
	}
	return 0
}

// splitEndpoint 拆 "地址:端口"，端口转数值；无法解析时端口为 -1。
func splitEndpoint(s string) (string, int) {
	host, ps, err := net.SplitHostPort(s)
	if err != nil {
		return s, -1
	}
	p, err := strconv.Atoi(ps)
	if err != nil {
		return host, -1
	}
	return host, p
}

// compareIP 按 IP 数值比较；任一侧解析不了时退回字符串比较。
func compareIP(a, b string) int {
	x, y := net.ParseIP(a), net.ParseIP(b)
	if x == nil || y == nil {
		return strings.Compare(a, b)
	}
	return bytes.Compare(x.To16(), y.To16())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
