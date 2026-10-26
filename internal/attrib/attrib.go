// Package attrib 把进程归因到"Agent/系统桶"：构建进程树，沿祖先链
// 匹配识别规则（AI Agent、本地模型运行时等），并按桶聚合资源总量。
package attrib

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/lomehong/reshmon/internal/model"
)

// Pattern 一条识别规则。Match 为小写子串，默认匹配进程 exe 名；
// "path:" 前缀匹配进程完整路径；"name:" 前缀精确匹配 exe 名
// （避免如 droid/android 这类子串误伤）。
type Pattern struct {
	Match string `json:"match"`
	Label string `json:"label"`
}

// DefaultPatterns 内置识别规则：常见 AI Agent / 编码工具 / 本地模型运行时。
// 用户可在 <数据目录>/agents.json 中追加。
func DefaultPatterns() []Pattern {
	p := func(m, l string) Pattern { return Pattern{Match: m, Label: l} }
	return []Pattern{
		p("zcode", "ZCode"),
		p("claude", "Claude"),
		p("codex", "Codex"),
		p("cursor", "Cursor"),
		p("windsurf", "Windsurf"),
		p("trae", "Trae"),
		p("augment", "Augment"),
		p("cline", "Cline"),
		p("gemini", "Gemini"),
		p("copilot", "Copilot"),
		p("qwen", "Qwen"),
		p("kimi", "Kimi"),
		p("name:droid.exe", "Droid"),
		p("qoder", "Qoder"),
		p("ollama", "Ollama"),
		p("lmstudio", "LM Studio"),
		p("lm studio", "LM Studio"),
		p("llamafile", "Llamafile"),
		p("llama-server", "Llama.cpp"),
		p("llama.cpp", "Llama.cpp"),
		p("msty", "Msty"),
		p("anythingllm", "AnythingLLM"),
		p("chatbox", "Chatbox"),
		p("comfyui", "ComfyUI"),
		p("sd-webui", "SD-WebUI"),
		p("stable-diffusion", "SD-WebUI"),
	}
}

// LoadUserPatterns 从 agents.json 读取用户自定义规则；文件不存在返回 nil。
func LoadUserPatterns(path string) ([]Pattern, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg struct {
		Agents []Pattern `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return cfg.Agents, nil
}

// sysRoots 归并为单一"系统服务"桶的根进程名。
var sysRoots = map[string]string{
	"services.exe":   "系统服务",
	"wininit.exe":    "系统服务",
	"lsass.exe":      "系统服务",
	"winlogon.exe":   "系统服务",
	"smss.exe":       "系统服务",
	"system":         "内核",
	"registry":       "内核",
	"idle":           "内核",
	"memcompression": "系统服务",
}

// Engine 进程归因引擎（有状态：sticky 表跨 tick 保持孤儿进程归属）。
type Engine struct {
	patterns []Pattern
	sticky   map[uint32]stickyEnt
}

// stickyEnt 孤儿进程的历史归属；create 为进程创建时间——PID 被复用
// （新进程 create 不同）时历史立即作废，不把新进程记到前任的 Agent 头上。
type stickyEnt struct {
	label  string
	create uint64
}

func New(patterns []Pattern) *Engine {
	return &Engine{patterns: patterns, sticky: map[uint32]stickyEnt{}}
}

func (e *Engine) match(name, path string) (string, bool) {
	n := strings.ToLower(name)
	for _, pt := range e.patterns {
		m := strings.ToLower(pt.Match)
		if m == "" {
			continue
		}
		switch {
		case strings.HasPrefix(m, "path:"):
			sub := strings.TrimPrefix(m, "path:")
			if path != "" && strings.Contains(strings.ToLower(path), sub) {
				return pt.Label, true
			}
		case strings.HasPrefix(m, "name:"):
			if n == strings.TrimPrefix(m, "name:") {
				return pt.Label, true
			}
		default:
			if strings.Contains(n, m) {
				return pt.Label, true
			}
		}
	}
	return "", false
}

// Attribute 为每个进程标注 Agent 归属（写回 procs[i].Agent）。
// 规则：从进程沿父链上溯，取离自身最近的识别规则命中（嵌套 Agent——如
// ZCode 里跑 Claude——各记各的账：Claude 及其子树归 Claude，ZCode 自身
// 与其余子树归 ZCode）；父链已死时沿用 sticky 历史（校验创建时间防 PID
// 复用）；否则按根进程名归入系统桶——系统桶优先于 sticky，防止陈旧的
// sticky 抢占系统服务进程。
func (e *Engine) Attribute(procs []model.ProcStat) {
	byPID := make(map[uint32]int, len(procs))
	for i := range procs {
		byPID[procs[i].PID] = i
	}
	for i := range procs {
		procs[i].Agent = e.labelFor(procs, i, byPID)
	}
	alive := make(map[uint32]uint64, len(procs))
	for i := range procs {
		pid := procs[i].PID
		alive[pid] = procs[i].Start
		if procs[i].Agent != "" {
			e.sticky[pid] = stickyEnt{label: procs[i].Agent, create: procs[i].Start}
		}
	}
	for pid := range e.sticky {
		if create, ok := alive[pid]; !ok || create != e.sticky[pid].create {
			delete(e.sticky, pid)
		}
	}
}

func (e *Engine) labelFor(procs []model.ProcStat, i int, byPID map[uint32]int) string {
	// 从自身沿父链上溯，chain[0]=自身，chain[len-1]=可达的最老祖先。
	// 父候选需创建时间不晚于子进程（剔除 PPID 被复用产生的假父子）。
	var chain []int
	cur := procs[i].PID
	seen := map[uint32]bool{}
	for depth := 0; depth < 128; depth++ {
		idx, ok := byPID[cur]
		if !ok || seen[cur] {
			break
		}
		seen[cur] = true
		chain = append(chain, idx)
		next := procs[idx].PPID
		if next == 0 {
			break
		}
		if pIdx, ok2 := byPID[next]; ok2 &&
			procs[pIdx].Start != 0 && procs[idx].Start != 0 && procs[pIdx].Start > procs[idx].Start {
			break
		}
		cur = next
	}
	// 1. 从自身向根找第一个识别规则命中：最近的 Agent 祖先优先。
	for j := 0; j < len(chain); j++ {
		q := &procs[chain[j]]
		if label, ok := e.match(q.Name, q.Path); ok {
			return label
		}
	}
	if len(chain) == 0 {
		return strings.ToLower(procs[i].Name)
	}
	root := strings.ToLower(procs[chain[len(chain)-1]].Name)
	// 2. 系统根归系统桶（优先于 sticky，防陈旧历史抢占系统进程）。
	if r, ok := sysRoots[root]; ok {
		return r
	}
	// 3. 孤儿（父链断裂，chain 只剩自身）沿用 sticky 历史；
	//    创建时间不一致 = PID 被复用，历史作废。
	if len(chain) == 1 {
		if s, ok := e.sticky[procs[i].PID]; ok && s.label != "" && s.create == procs[i].Start {
			return s.label
		}
	}
	// 4. 根名桶。
	return root
}

// Aggregate 按归属标签聚合资源。进程级 GPU 单值上限按 100 截断后累加，
// 多进程共享一块 GPU 时桶值可超 100，属预期口径。
func Aggregate(procs []model.ProcStat) []model.AgentStat {
	m := map[string]*model.AgentStat{}
	for _, p := range procs {
		label := p.Agent
		if label == "" {
			label = "(未归类)"
		}
		a := m[label]
		if a == nil {
			a = &model.AgentStat{Name: label}
			m[label] = a
		}
		a.CpuPct += p.CpuPct
		a.MemPrivB += p.MemPrivB
		a.ReadBps += p.ReadBps
		a.WriteBps += p.WriteBps
		if p.GpuPct > 100 {
			a.GpuPct += 100
		} else {
			a.GpuPct += p.GpuPct
		}
		a.VramB += p.VramB
		a.Procs++
	}
	out := make([]model.AgentStat, 0, len(m))
	for _, a := range m {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CpuPct != out[j].CpuPct {
			return out[i].CpuPct > out[j].CpuPct
		}
		return out[i].MemPrivB > out[j].MemPrivB
	})
	return out
}
