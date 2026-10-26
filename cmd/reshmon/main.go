//go:build windows

// reshmon —— Windows 资源健康监护小工具。
//
// 用法：
//
//	reshmon once    [-json] [-obs 800ms]      单次采样并打印（退出码 0/1/2=健康/亚健康/饱和，3=执行错误）
//	reshmon live    [--data D] [--no-record] [--interval 1s]  终端实时面板（默认记录历史）
//	reshmon serve   [--addr 127.0.0.1:8964]   Web 面板 + 常驻记录
//	reshmon report  [--day 2026-10-26 | --last 24h]  回溯分析历史
//	reshmon version
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/lomehong/reshmon/internal/app"
	"github.com/lomehong/reshmon/internal/health"
	"github.com/lomehong/reshmon/internal/mcp"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/report"
	"github.com/lomehong/reshmon/internal/sampler"
	"github.com/lomehong/reshmon/internal/syswin"
	"github.com/lomehong/reshmon/internal/tui"
	"github.com/lomehong/reshmon/internal/webui"
)

func main() {
	syswin.SetupConsole()
	if len(os.Args) < 2 {
		usage()
		os.Exit(3)
	}
	var err error
	switch os.Args[1] {
	case "once":
		code, cerr := cmdOnce(os.Args[2:])
		if cerr != nil {
			err = cerr
			break
		}
		if code != 0 {
			os.Exit(code) // cmdOnce 的 defer 已在其 return 时执行完
		}
	case "version":
		fmt.Println("reshmon", app.Version)
	case "live":
		err = cmdLive(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(3)
	}
}

func usage() {
	fmt.Print(`reshmon —— Windows 资源健康监护（CPU/内存/提交/磁盘/GPU + Agent 归因）

用法:
  reshmon once    [-json] [-obs 800ms] [-data 目录]     单次采样打印，退出码 0/1/2=健康/亚健康/饱和，3=执行错误
  reshmon live    [-data 目录] [-no-record] [-interval 1s]   终端实时面板
  reshmon serve   [-addr 127.0.0.1:8964] [-data 目录] [-no-record] [-interval 1s]   Web 面板（默认同时记录历史）
  reshmon mcp     [-data 目录]   MCP 服务器（stdio）——让 Agent 自查资源后决策并发
  reshmon report  [-day 2026-10-26 | -last 24h]         回溯分析已记录的历史
  reshmon version

数据目录默认 %LOCALAPPDATA%\reshmon（config.json 阈值 / agents.json 识别规则）
`)
}

// cmdOnce 单次采样：双帧采集（PDH 速率需要两帧 + 进程差分），obs 为观察窗。
// 退出码与 README 契约一致：0=健康 1=亚健康 2=饱和——JSON 输出同样适用
// （健康结论编码在输出的 health 字段，脚本两种方式都能接）。
func cmdOnce(args []string) (int, error) {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	obs := fs.Duration("obs", 800*time.Millisecond, "观察窗时长（差分基线）")
	asJSON := fs.Bool("json", false, "输出 JSON（机器可读）")
	dataDir := fs.String("data", "", "数据目录（读 agents.json/config.json）")
	_ = fs.Parse(args)

	cfg, warns := app.LoadConfig(*dataDir)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "⚠", w)
	}
	s, err := sampler.New(*obs, cfg.Patterns)
	if err != nil {
		return 3, err
	}
	defer s.Close()
	s.Tick() // 基线帧
	time.Sleep(*obs)
	snap := s.Tick()

	rep := health.NewJudge(cfg.Thresholds, 1).Evaluate([]model.Totals{app.TotalsFrom(snap)})
	health.ApplyDegrade(&rep, snap.CapErrors)
	if *asJSON {
		if err := printJSON(snap, rep); err != nil {
			return 3, err
		}
	} else {
		printHuman(snap, rep)
	}
	switch rep.Overall {
	case model.StatusSat:
		return 2, nil
	case model.StatusWarn:
		return 1, nil
	}
	return 0, nil
}

type onceJSON struct {
	TS        string            `json:"ts"`
	Interval  string            `json:"interval"`
	CPU       model.CPUStat     `json:"cpu"`
	Mem       model.MemStat     `json:"mem"`
	Disks     []model.DiskStat  `json:"disks"`
	GPUs      []model.GPUStat   `json:"gpus"`
	NICs      []model.NICStat   `json:"nics"`
	Agents    []model.AgentStat `json:"agents"`
	Procs     []model.ProcStat  `json:"procs"`
	Health    health.Report     `json:"health"`
	CapErrors []string          `json:"capErrors,omitempty"`
}

func printJSON(s *model.Snapshot, rep health.Report) error {
	procs := append([]model.ProcStat(nil), s.Procs...)
	sort.Slice(procs, func(i, j int) bool { return procs[i].CpuPct > procs[j].CpuPct })
	out := onceJSON{
		TS: s.TS.Format(time.RFC3339), Interval: s.Interval.String(),
		CPU: s.CPU, Mem: s.Mem, Disks: s.Disks, GPUs: s.GPUs, NICs: s.NICs,
		Agents: s.Agents, Procs: procs, Health: rep, CapErrors: s.CapErrors,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printHuman(s *model.Snapshot, rep health.Report) {
	fmt.Printf("reshmon 快照  %s   采样窗 %s\n", s.TS.Format("2006-01-02 15:04:05"), s.Interval.Truncate(time.Millisecond))
	fmt.Printf("总体: %s\n", statusBadge(rep.Overall))
	if len(rep.Reasons) > 0 {
		for _, r := range rep.Reasons {
			fmt.Printf("  ⚠ %s\n", r)
		}
	}
	fmt.Printf("\nCPU      %s  %d 核\n", bar(s.CPU.BusyPct), s.CPU.Cores)
	usedB := float64(s.Mem.PhysTotalB - s.Mem.PhysAvailB)
	fmt.Printf("内存     %s  物理 %s / %s   提交 %s / %s\n",
		bar(s.Mem.PhysUsedPct), model.FmtBytes(usedB), model.FmtBytes(float64(s.Mem.PhysTotalB)),
		model.FmtBytes(float64(s.Mem.CommitTotalB)), model.FmtBytes(float64(s.Mem.CommitLimitB)))
	for _, d := range s.Disks {
		fmt.Printf("磁盘 %-8s %s  读 %8s/s 写 %8s/s  延迟 R%4.1f W%4.1fms  队列 %4.1f\n",
			labelOf(d.Letters, d.ID), bar(d.BusyPct),
			model.FmtBytes(d.ReadBps), model.FmtBytes(d.WriteBps),
			d.ReadLatMs, d.WriteLatMs, d.Queue)
	}
	for _, g := range s.GPUs {
		temp := ""
		if g.TempC >= 0 {
			temp = fmt.Sprintf("  温度 %d℃", g.TempC)
		}
		vram := ""
		if g.DedicatedTotalB > 0 {
			vram = fmt.Sprintf("  显存 %s / %s", model.FmtBytes(float64(g.DedicatedUsedB)), model.FmtBytes(float64(g.DedicatedTotalB)))
		}
		fmt.Printf("GPU  %s\n      %s  引擎利用率 %.1f%%  共享占用 %s%s\n",
			g.Name, bar(g.UtilCapPct), g.UtilPct, model.FmtBytes(float64(g.SharedUsedB)), temp+vram)
	}
	var nr, ns float64
	for _, n := range s.NICs {
		nr += n.RecvBps
		ns += n.SentBps
	}
	fmt.Printf("网络     收 %s/s  发 %s/s\n", model.FmtBytes(nr), model.FmtBytes(ns))

	if len(s.Agents) > 0 {
		fmt.Printf("\n%-24s %8s %10s %12s %8s\n", "Agent/桶（按 CPU）", "CPU%", "内存(私有)", "磁盘IO", "GPU%")
		for i, a := range s.Agents {
			if i >= 10 && a.CpuPct < 0.5 {
				break
			}
			fmt.Printf("%-24s %7.1f %10s %10s/s %8.1f\n",
				a.Name, a.CpuPct, model.FmtBytes(float64(a.MemPrivB)),
				model.FmtBytes(a.ReadBps+a.WriteBps), a.GpuPct)
		}
	}
	if len(s.CapErrors) > 0 {
		fmt.Println("\n采集降级:")
		for _, e := range s.CapErrors {
			fmt.Printf("  - %s\n", e)
		}
	}
}

func labelOf(letters, id string) string {
	if letters != "" {
		return letters
	}
	return id
}

func bar(pct float64) string {
	const w = 20
	f := int(pct / 100 * w)
	if f < 0 {
		f = 0
	}
	if f > w {
		f = w
	}
	b := make([]rune, w)
	for i := range b {
		if i < f {
			b[i] = '█'
		} else {
			b[i] = '░'
		}
	}
	return fmt.Sprintf("[%s] %5.1f%%", string(b), pct)
}

func statusBadge(st string) string {
	switch st {
	case model.StatusOK:
		return "● 健康"
	case model.StatusWarn:
		return "▲ 亚健康"
	case model.StatusSat:
		return "■ 饱和"
	}
	return st
}

// cmdLive 终端实时面板（默认记录历史）。
func cmdLive(args []string) error {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	dataDir := fs.String("data", "", "数据目录")
	noRecord := fs.Bool("no-record", false, "只看不落盘")
	interval := fs.Duration("interval", 0, "采样间隔（默认读 config，初始 2s）")
	_ = fs.Parse(args)

	cfg, warns := app.LoadConfig(*dataDir)
	if *noRecord {
		cfg.Record = false
	}
	if *interval > 0 {
		cfg.Interval = *interval
		cfg.DeriveForInterval()
	}
	a, err := app.Start(cfg)
	if err != nil {
		return err
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "⚠", w)
		a.AddErr(w)
	}
	defer a.Close()
	return tui.Run(a, cfg.Interval)
}

// cmdServe Web 面板 + 常驻记录。
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8964", "监听地址")
	dataDir := fs.String("data", "", "数据目录")
	noRecord := fs.Bool("no-record", false, "只看不落盘")
	interval := fs.Duration("interval", 0, "采样间隔（默认读 config，初始 2s）")
	_ = fs.Parse(args)

	cfg, warns := app.LoadConfig(*dataDir)
	if *noRecord {
		cfg.Record = false
	}
	if *interval > 0 {
		cfg.Interval = *interval
		cfg.DeriveForInterval()
	}
	a, err := app.Start(cfg)
	if err != nil {
		return err
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "⚠", w)
		a.AddErr(w)
	}
	defer a.Close()

	fmt.Printf("reshmon %s 面板: http://%s/   (Ctrl+C 停止)\n数据目录: %s   历史记录: %v\n", app.Version, *addr, cfg.DataDir, cfg.Record)
	return webui.Serve(a, *addr)
}

// cmdMCP MCP 服务器（stdio）：供 AI Agent 接入——发起任务前自查资源
// （快照/Agent 账本/趋势/事件），自行决定并发与时机。
// stdout 是协议通道，一切诊断输出走 stderr。内嵌只读采样（不落盘、
// 不抢数据目录写锁），多个 Agent 各自启动互不干扰。
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dataDir := fs.String("data", "", "数据目录")
	interval := fs.Duration("interval", 0, "采样间隔（默认读 config，初始 2s）")
	_ = fs.Parse(args)

	cfg, warns := app.LoadConfig(*dataDir)
	cfg.Record = false // 只读：不落盘、不占数据目录锁，与常驻 serve 共存
	if *interval > 0 {
		cfg.Interval = *interval
		cfg.DeriveForInterval()
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "⚠", w)
	}
	a, err := app.Start(cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	return mcp.Serve(a, app.Version, os.Stdin, os.Stdout, os.Stderr)
}

// cmdReport 离线回溯分析。
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dataDir := fs.String("data", "", "数据目录")
	day := fs.String("day", "", "分析某天，如 2026-10-26（默认今天）")
	last := fs.Duration("last", 0, "分析最近时长，如 24h / 6h（优先于 -day）")
	_ = fs.Parse(args)

	cfg, warns := app.LoadConfig(*dataDir)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "⚠", w)
	}
	now := time.Now()
	var from, to time.Time
	if *last > 0 {
		from, to = now.Add(-*last), now
	} else if *day != "" {
		d, err := time.ParseInLocation("2006-01-02", *day, time.Local)
		if err != nil {
			return fmt.Errorf("-day 格式应为 2026-10-26: %w", err)
		}
		from, to = d, d.Add(24*time.Hour)
		if to.After(now) {
			to = now
		}
	} else {
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		to = now
	}
	out, err := report.Run(cfg.DataDir, from, to)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
