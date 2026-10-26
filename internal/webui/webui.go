//go:build windows

// Package webui 提供 reshmon 的 Web 面板：嵌入式单页 + JSON API。
// 面板读环形缓冲（最近约 2 小时），长历史回溯用 reshmon report。
package webui

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/lomehong/reshmon/internal/app"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
	"github.com/lomehong/reshmon/internal/syswin"
)

//go:embed index.html
var indexHTML []byte

const version = "0.1.0"

// Serve 启动面板（阻塞）。
func Serve(a *app.App, addr string) error {
	srv := &http.Server{Addr: addr, Handler: Handler(a), ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

// Handler 面板路由。
func Handler(a *app.App) http.Handler {
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
				"version":   version,
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
			"errors":    a.Errors,
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		sec := 300
		if v, err := strconv.Atoi(r.URL.Query().Get("sec")); err == nil && v > 0 && v <= 7200 {
			sec = v
		}
		cutoff := time.Now().Add(-time.Duration(sec) * time.Second).UnixMilli()
		est := sec/int(a.Cfg.Interval.Seconds()) + 8
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
		incs, err := store.LoadIncidents(a.Cfg.DataDir, time.Now().Add(-7*24*time.Hour), time.Now().Add(time.Minute))
		if err != nil {
			incs = nil
		}
		if len(incs) > n {
			incs = incs[len(incs)-n:]
		}
		// 内存中的最近事件兜底（未开启记录时也有）
		if len(incs) == 0 {
			for _, inc := range a.Incidents() {
				incs = append(incs, inc)
			}
		}
		writeJSON(w, map[string]any{"incidents": incs})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
