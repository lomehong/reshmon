// Package report 对已落盘的历史做离线回溯分析：
// 各维度峰值窗口 + 峰值时刻归因 + Agent 汇总 + 事件时间线。
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

type dimKind int

const (
	dimCPU dimKind = iota
	dimMem
	dimIO
	dimGPU
)

// Run 分析 [from,to] 并返回人读报告文本。
func Run(dataDir string, from, to time.Time) (string, error) {
	totals, warns, err := store.LoadTotals(dataDir, from, to)
	if err != nil {
		return "", err
	}
	if len(totals) == 0 {
		return fmt.Sprintf("%s ~ %s 范围内没有历史记录。serve/live 默认开启记录（--no-record 可关）。", from.Format("01-02 15:04"), to.Format("01-02 15:04")), nil
	}
	procs, pwarns, _ := store.LoadProcs(dataDir, from, to)
	warns = append(warns, pwarns...)
	incs, _ := store.LoadIncidents(dataDir, from, to)

	var b strings.Builder
	span := time.Duration(totals[len(totals)-1].T-totals[0].T) * time.Millisecond
	fmt.Fprintf(&b, "reshmon 回溯报告  %s ~ %s  样本 %d 条  跨度 %s\n",
		time.UnixMilli(totals[0].T).Format("01-02 15:04"), time.UnixMilli(totals[len(totals)-1].T).Format("01-02 15:04"),
		len(totals), span.Truncate(time.Second))
	// 数据缺口必须显式说明：缺失的进程/事件证据段不能被误读为"当时无嫌疑"。
	for _, w := range warns {
		fmt.Fprintf(&b, "⚠ %s\n", w)
	}

	// 总体健康分布
	dist := map[string]int{}
	for _, t := range totals {
		if t.Health != nil {
			if st, ok := t.Health["overall"]; ok {
				dist[st]++
			}
		}
	}
	if len(dist) > 0 {
		fmt.Fprintf(&b, "总体健康分布: ")
		for _, st := range []string{model.StatusOK, model.StatusWarn, model.StatusSat} {
			if dist[st] > 0 {
				fmt.Fprintf(&b, "%s %d (%.0f%%)  ", model.StatusCN(st), dist[st], float64(dist[st])/float64(len(totals))*100)
			}
		}
		b.WriteString("\n")
	}

	W := 15 // 约 30s @2s 采样
	if len(totals) < W {
		W = len(totals)
	}

	// 标量维度峰值
	section(&b, totals, procs, W, "CPU", dimCPU, func(t model.Totals) float64 { return t.CPU })
	section(&b, totals, procs, W, "内存", dimMem, func(t model.Totals) float64 { return t.MemUsedPct })
	section(&b, totals, procs, W, "提交内存", dimMem, func(t model.Totals) float64 { return t.CommitPct })

	// 磁盘（按物理盘身份键聚合：盘符增减不拆裂序列）
	dids := map[string]string{}
	for _, t := range totals {
		for _, d := range t.Disks {
			k := model.DiskKey(d.ID)
			if d.L != "" {
				dids[k] = d.L
			} else if _, ok := dids[k]; !ok {
				dids[k] = k
			}
		}
	}
	for _, id := range sortedKeys(dids) {
		section(&b, totals, procs, W, "磁盘 "+dids[id], dimIO, func(t model.Totals) float64 {
			for _, d := range t.Disks {
				if model.DiskKey(d.ID) == id {
					return d.Busy
				}
			}
			return 0
		})
	}
	// GPU（按 LUID 身份键聚合，显示名取自任一带名字的记录）
	gnames := map[string]string{}
	gkeys := map[string]bool{}
	for _, t := range totals {
		for _, g := range t.GPUs {
			gkeys[g.Key] = true
			if g.Name != "" {
				gnames[g.Key] = g.Name
			}
		}
	}
	for _, k := range sortedKeysB(gkeys) {
		label := gnames[k]
		if label == "" {
			label = k
		}
		section(&b, totals, procs, W, "GPU "+label, dimGPU, func(t model.Totals) float64 {
			for _, g := range t.GPUs {
				if g.Key == k {
					return g.Util
				}
			}
			return 0
		})
	}

	// Agent 汇总
	agentSummary(&b, totals)

	// 事件时间线
	if len(incs) > 0 {
		fmt.Fprintf(&b, "\n▍健康事件（%d 条）\n", len(incs))
		n := len(incs)
		if n > 30 {
			incs = incs[n-30:]
			b.WriteString("  …（仅最近 30 条）\n")
		}
		for _, i := range incs {
			fmt.Fprintf(&b, "  %s  %-20s %s → %s", time.UnixMilli(i.T).Format("01-02 15:04:05"),
				i.Dim, model.StatusCN(i.From), model.StatusCN(i.To))
			if i.Detail != "" {
				fmt.Fprintf(&b, "   %s", i.Detail)
			}
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

func section(b *strings.Builder, totals []model.Totals, procs []store.ProcsRec, W int, name string, kind dimKind, get func(model.Totals) float64) {
	vals := make([]float64, len(totals))
	for i, t := range totals {
		vals[i] = get(t)
	}
	wins := topWindows(vals, W, 3)
	if len(wins) == 0 {
		return
	}
	fmt.Fprintf(b, "\n▍%s 峰值窗口（滑动 %d 样本均值，Top %d）\n", name, W, len(wins))
	for wi, w := range wins {
		sum, peak, pi := 0.0, vals[w[0]], w[0]
		for i := w[0]; i < w[1]; i++ {
			sum += vals[i]
			if vals[i] > peak {
				peak, pi = vals[i], i
			}
		}
		mean := sum / float64(w[1]-w[0])
		fmt.Fprintf(b, "  %d. %s ~ %s  均值 %.1f%%  峰值 %.1f%% @%s\n", wi+1,
			time.UnixMilli(totals[w[0]].T).Format("15:04:05"), time.UnixMilli(totals[w[1]-1].T).Format("15:04:05"),
			mean, peak, time.UnixMilli(totals[pi].T).Format("15:04:05"))
		// 峰值时刻的 Agent 归因
		at := totals[pi].Agents
		rank := append([]model.AgentStat(nil), at...)
		sort.Slice(rank, func(i, j int) bool { return agentDim(rank[i], kind) > agentDim(rank[j], kind) })
		parts := []string{}
		for i, a := range rank {
			if i >= 3 || agentDim(a, kind) <= 0 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %s", a.Name, agentVal(a, kind)))
		}
		if len(parts) > 0 {
			fmt.Fprintf(b, "     当时 Agent: %s\n", strings.Join(parts, " · "))
		}
		// 最近一条进程切片的对应维度 Top
		if rec := nearestProcs(procs, totals[pi].T); rec != nil {
			top := append([]model.ProcStat(nil), rec.Top...)
			sort.Slice(top, func(i, j int) bool { return procDim(top[i], kind) > procDim(top[j], kind) })
			names := []string{}
			for i, p := range top {
				if i >= 5 || procDim(p, kind) <= 0 {
					break
				}
				names = append(names, fmt.Sprintf("%s(%s)", p.Name, procVal(p, kind)))
			}
			if len(names) > 0 {
				fmt.Fprintf(b, "     当时进程[%s]: %s\n", time.UnixMilli(rec.T).Format("15:04:05"), strings.Join(names, " "))
			}
		}
	}
}

func agentDim(a model.AgentStat, k dimKind) float64 {
	switch k {
	case dimCPU:
		return a.CpuPct
	case dimMem:
		return float64(a.MemPrivB) / 1e9
	case dimIO:
		return (a.ReadBps + a.WriteBps) / 1e6
	default:
		return a.GpuPct
	}
}

func agentVal(a model.AgentStat, k dimKind) string {
	switch k {
	case dimCPU:
		return fmt.Sprintf("%.1f%%", a.CpuPct)
	case dimMem:
		return model.FmtBytes(float64(a.MemPrivB))
	case dimIO:
		return fmt.Sprintf("%s/s", model.FmtBytes(a.ReadBps+a.WriteBps))
	default:
		return fmt.Sprintf("%.1f%%", a.GpuPct)
	}
}

func procDim(p model.ProcStat, k dimKind) float64 {
	switch k {
	case dimCPU:
		return p.CpuPct
	case dimMem:
		return float64(p.MemPrivB) / 1e9
	case dimIO:
		return (p.ReadBps + p.WriteBps) / 1e6
	default:
		return p.GpuPct
	}
}

func procVal(p model.ProcStat, k dimKind) string {
	switch k {
	case dimCPU:
		return fmt.Sprintf("%.1f%%", p.CpuPct)
	case dimMem:
		return model.FmtBytes(float64(p.MemPrivB))
	case dimIO:
		return fmt.Sprintf("%s/s", model.FmtBytes(p.ReadBps+p.WriteBps))
	default:
		return fmt.Sprintf("%.1f%%", p.GpuPct)
	}
}

// nearestProcs 找时间上最接近 t 的进程切片记录（前后皆可）。
func nearestProcs(procs []store.ProcsRec, t int64) *store.ProcsRec {
	var best *store.ProcsRec
	bestDiff := int64(1<<62 - 1)
	for i := range procs {
		d := procs[i].T - t
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			bestDiff = d
			best = &procs[i]
		}
	}
	return best
}

// topWindows 滑动均值找 Top-k 窗口；选中窗口向两侧各扩一个窗宽作为
// 排除区，避免同一事件被相邻窗口重复选中（均值≤0 的不选）。
func topWindows(vals []float64, W, k int) [][2]int {
	n := len(vals)
	if n == 0 || W <= 0 {
		return nil
	}
	pre := make([]float64, n+1)
	for i, v := range vals {
		pre[i+1] = pre[i] + v
	}
	used := make([]bool, n)
	var out [][2]int
	for len(out) < k {
		best, bi := -1.0, -1
		for i := 0; i+W <= n; i++ {
			if used[i] {
				continue
			}
			m := (pre[i+W] - pre[i]) / float64(W)
			if m > best {
				best, bi = m, i
			}
		}
		if bi < 0 || best <= 0 {
			break
		}
		for j := bi - W; j < bi+2*W; j++ {
			if j >= 0 && j < n {
				used[j] = true
			}
		}
		out = append(out, [2]int{bi, bi + W})
	}
	return out
}

func agentSummary(b *strings.Builder, totals []model.Totals) {
	type acc struct {
		n                     int
		cpu, mem, io, gpu     float64
		maxCPU, maxMem, maxIO float64
		maxGPU                float64
	}
	m := map[string]*acc{}
	for _, t := range totals {
		for _, a := range t.Agents {
			x := m[a.Name]
			if x == nil {
				x = &acc{}
				m[a.Name] = x
			}
			io := (a.ReadBps + a.WriteBps) / 1e6
			x.n++
			x.cpu += a.CpuPct
			x.mem += float64(a.MemPrivB) / 1e9
			x.io += io
			x.gpu += a.GpuPct
			if a.CpuPct > x.maxCPU {
				x.maxCPU = a.CpuPct
			}
			if float64(a.MemPrivB)/1e9 > x.maxMem {
				x.maxMem = float64(a.MemPrivB) / 1e9
			}
			if io > x.maxIO {
				x.maxIO = io
			}
			if a.GpuPct > x.maxGPU {
				x.maxGPU = a.GpuPct
			}
		}
	}
	type row struct {
		name      string
		cpu, mcpu float64
		mem, mmem float64
		io, mio   float64
		gpu, mgpu float64
	}
	var rows []row
	for name, x := range m {
		n := float64(x.n)
		r := row{name: name, cpu: x.cpu / n, mcpu: x.maxCPU, mem: x.mem / n, mmem: x.maxMem,
			io: x.io / n, mio: x.maxIO, gpu: x.gpu / n, mgpu: x.maxGPU}
		// 过滤全程无感的桶
		if r.mcpu < 1 && r.mmem < 0.2 && r.mio < 0.2 && r.mgpu < 1 {
			continue
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cpu > rows[j].cpu })
	if len(rows) > 15 {
		rows = rows[:15]
	}
	fmt.Fprintf(b, "\n▍Agent 汇总（均值 / 峰值，按 CPU 均值排序）\n")
	fmt.Fprintf(b, "  %-24s %18s %20s %18s\n", "名称", "CPU%", "内存GB", "磁盘MB/s")
	for _, r := range rows {
		fmt.Fprintf(b, "  %-24s %7.1f / %7.1f  %7.1f / %7.1f  %6.1f / %6.1f  gpu %5.1f/%5.1f\n",
			r.name, r.cpu, r.mcpu, r.mem, r.mmem, r.io, r.mio, r.gpu, r.mgpu)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysB(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
