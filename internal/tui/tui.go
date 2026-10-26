//go:build windows

// Package tui 终端实时面板：备用屏 + ANSI 颜色，Ctrl+C 退出。
package tui

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/lomehong/reshmon/internal/app"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/syswin"
)

const (
	cReset = "\x1b[0m"
	cBold  = "\x1b[1m"
	cDim   = "\x1b[2m"
	cRed   = "\x1b[31m"
	cGreen = "\x1b[32m"
	cYel   = "\x1b[33m"
	cBlue  = "\x1b[34m"
)

func stColor(st string) string {
	switch st {
	case model.StatusSat:
		return cRed
	case model.StatusWarn:
		return cYel
	default:
		return cGreen
	}
}

// Run 阻塞渲染直到 Ctrl+C。
func Run(a *app.App, refresh time.Duration) error {
	if refresh < 500*time.Millisecond {
		refresh = 500 * time.Millisecond
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	render(a)
	t := time.NewTicker(refresh)
	defer t.Stop()
	for {
		select {
		case <-sig:
			return nil
		case <-t.C:
			render(a)
		}
	}
}

func render(a *app.App) {
	snap, rep := a.Current()
	if snap == nil { // 首张快照尚未就绪
		fmt.Print("\x1b[H\x1b[2Jreshmon 启动中，等待首个采样…\n")
		return
	}
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "%sreshmon%s  %s  %s  开机 %s\n", cBold, cReset, host,
		snap.TS.Format("2006-01-02 15:04:05"), fmtDur(syswin.UptimeMs()/1000))
	ov := stColor(rep.Overall)
	fmt.Fprintf(&b, "总体 %s● %s%s", ov, model.StatusCN(rep.Overall), cReset)
	if len(rep.Reasons) > 0 {
		fmt.Fprintf(&b, "   %s⚠ %s%s", ov, strings.Join(rep.Reasons, "；"), cReset)
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "CPU   %s  %d 核  ", bar(snap.CPU.BusyPct, snap.CPU.BusyPct >= 85, snap.CPU.BusyPct >= 65), snap.CPU.Cores)
	// 每核负载小图
	b.WriteString(cDim)
	for i, c := range snap.CPU.PerCore {
		if i > 0 && i%32 == 0 {
			b.WriteString("\n                     ")
		}
		switch {
		case c >= 85:
			b.WriteString("\x1b[31m▮")
		case c >= 60:
			b.WriteString("\x1b[33m▮")
		case c >= 25:
			b.WriteString("\x1b[32m▮")
		default:
			b.WriteString("▯")
		}
	}
	b.WriteString(cReset + "\n")

	used := float64(snap.Mem.PhysTotalB - snap.Mem.PhysAvailB)
	fmt.Fprintf(&b, "内存  %s  %s / %s   提交 %s / %s (%.0f%%)\n",
		bar(snap.Mem.PhysUsedPct, snap.Mem.PhysUsedPct >= 92, snap.Mem.PhysUsedPct >= 85),
		model.FmtBytes(used), model.FmtBytes(float64(snap.Mem.PhysTotalB)),
		model.FmtBytes(float64(snap.Mem.CommitTotalB)), model.FmtBytes(float64(snap.Mem.CommitLimitB)), snap.Mem.CommitPct)

	for _, d := range snap.Disks {
		fmt.Fprintf(&b, "磁盘 %-8s %s  读 %8s/s 写 %8s/s  延迟 R%4.1f W%4.1fms  队列 %4.1f\n",
			lbl(d.Letters, d.ID), bar(d.BusyPct, d.BusyPct >= 90, d.BusyPct >= 75),
			model.FmtBytes(d.ReadBps), model.FmtBytes(d.WriteBps), d.ReadLatMs, d.WriteLatMs, d.Queue)
	}
	for _, g := range snap.GPUs {
		temp := ""
		if g.TempC >= 0 {
			temp = fmt.Sprintf("  温度 %d℃", g.TempC)
		}
		vram := ""
		if g.DedicatedTotalB > 0 {
			vram = fmt.Sprintf("  显存 %s / %s", model.FmtBytes(float64(g.DedicatedUsedB)), model.FmtBytes(float64(g.DedicatedTotalB)))
		}
		fmt.Fprintf(&b, "GPU %s\n     %s  引擎 %.1f%%  共享 %s%s%s\n", g.Name,
			bar(g.UtilCapPct, g.UtilCapPct >= 95, g.UtilCapPct >= 80), g.UtilPct,
			model.FmtBytes(float64(g.SharedUsedB)), vram, temp)
	}
	var nr, ns float64
	for _, n := range snap.NICs {
		nr += n.RecvBps
		ns += n.SentBps
	}
	fmt.Fprintf(&b, "网络  收 %s/s  发 %s/s\n\n", model.FmtBytes(nr), model.FmtBytes(ns))

	if len(snap.Agents) > 0 {
		b.WriteString(cBold + "Agent / 桶（按 CPU）\n" + cReset)
		fmt.Fprintf(&b, "  %-26s %8s %10s %12s %8s\n", "名称", "CPU%", "内存", "磁盘IO", "GPU%")
		for i, ag := range snap.Agents {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "  %-26s %7.1f %10s %10s/s %8.1f\n", ag.Name, ag.CpuPct,
				model.FmtBytes(float64(ag.MemPrivB)), model.FmtBytes(ag.ReadBps+ag.WriteBps), ag.GpuPct)
		}
		b.WriteString("\n")
	}

	procs := append([]model.ProcStat(nil), snap.Procs...)
	sort.Slice(procs, func(i, j int) bool { return procs[i].CpuPct > procs[j].CpuPct })
	b.WriteString(cBold + "进程 Top（CPU）\n" + cReset)
	for i, p := range procs {
		if i >= 12 || p.CpuPct <= 0 {
			break
		}
		agent := p.Agent
		if agent == "" {
			agent = "-"
		}
		fmt.Fprintf(&b, "  %6d %-24s %-14s %6.1f%%  %8s  IO %8s/s\n",
			p.PID, p.Name, agent, p.CpuPct, model.FmtBytes(float64(p.MemPrivB)),
			model.FmtBytes(p.ReadBps+p.WriteBps))
	}

	incs := a.Incidents()
	if len(incs) > 0 {
		b.WriteString("\n" + cBold + "最近事件\n" + cReset)
		n := len(incs)
		if n > 5 {
			incs = incs[n-5:]
		}
		for _, i := range incs {
			fmt.Fprintf(&b, "  %s %-22s %s→%s %s\n", time.UnixMilli(i.T).Format("15:04:05"),
				i.Dim, model.StatusCN(i.From), stColor(i.To)+model.StatusCN(i.To)+cReset, cDim+i.Detail+cReset)
		}
	}

	// 采集降级与运行错误必须可见：写盘失败/GPU 计数器缺失不能静默
	if degr := degradeLines(snap, a); len(degr) > 0 {
		b.WriteString("\n" + cYel)
		for _, d := range degr {
			fmt.Fprintf(&b, "⚠ %s\n", d)
		}
		b.WriteString(cReset)
	}

	b.WriteString("\n" + cDim + "数据目录 " + a.Cfg.DataDir)
	if !a.Cfg.Record {
		b.WriteString("（未记录）")
	}
	b.WriteString("  ·  Ctrl+C 退出" + cReset + "\n")
	os.Stdout.WriteString(b.String())
}

func degradeLines(snap *model.Snapshot, a *app.App) []string {
	var out []string
	for _, e := range snap.CapErrors {
		out = append(out, "采集降级："+e)
	}
	for _, e := range a.Errs() {
		out = append(out, e)
	}
	if len(out) > 4 {
		out = out[len(out)-4:]
	}
	return out
}

func lbl(letters, id string) string {
	if letters != "" {
		return letters
	}
	return id
}

func bar(pct float64, crit, warn bool) string {
	const w = 20
	f := int(pct / 100 * w)
	if f < 0 {
		f = 0
	}
	if f > w {
		f = w
	}
	color := cGreen
	if warn {
		color = cYel
	}
	if crit {
		color = cRed
	}
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(color)
	for i := 0; i < w; i++ {
		if i < f {
			b.WriteString("█")
		} else {
			b.WriteString("░")
		}
	}
	b.WriteString(cReset)
	fmt.Fprintf(&b, "] %5.1f%%", pct)
	return b.String()
}

func fmtDur(sec uint64) string {
	d := sec / 86400
	h := sec % 86400 / 3600
	m := sec % 3600 / 60
	if d > 0 {
		return fmt.Sprintf("%d天%d小时", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%d小时%d分", h, m)
	}
	return fmt.Sprintf("%d分", m)
}
