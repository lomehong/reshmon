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
// 加 "path:" 前缀则匹配进程完整路径。
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
		p("droid", "Droid"),
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
	sticky   map[uint32]string
}

func New(patterns []Pattern) *Engine {
	return &Engine{patterns: patterns, sticky: map[uint32]string{}}
}

func (e *Engine) match(name, path string) (string, bool) {
	n := strings.ToLower(name)
	for _, pt := range e.patterns {
		m := strings.ToLower(pt.Match)
		if m == "" {
			continue
		}
		if strings.HasPrefix(m, "path:") {
			sub := strings.TrimPrefix(m, "path:")
			if path != "" && strings.Contains(strings.ToLower(path), sub) {
				return pt.Label, true
			}
			continue
		}
		if strings.Contains(n, m) {
			return pt.Label, true
		}
	}
	return "", false
}

// Attribute 为每个进程标注 Agent 归属（写回 procs[i].Agent）。
// 规则：从进程沿父链上溯到根，链上任意节点命中识别规则则归入该 Agent
// （最靠近根的命中优先，使"Agent 主进程→编译器→测试"整棵树算一笔账）；
// 父链已死或无命中时沿用 sticky 历史；否则按根进程名归入系统桶。
func (e *Engine) Attribute(procs []model.ProcStat) {
	byPID := make(map[uint32]int, len(procs))
	for i := range procs {
		byPID[procs[i].PID] = i
	}
	for i := range procs {
		procs[i].Agent = e.labelFor(procs, i, byPID)
	}
	alive := make(map[uint32]struct{}, len(procs))
	for i := range procs {
		pid := procs[i].PID
		alive[pid] = struct{}{}
		if procs[i].Agent != "" {
			e.sticky[pid] = procs[i].Agent
		}
	}
	for pid := range e.sticky {
		if _, ok := alive[pid]; !ok {
			delete(e.sticky, pid)
		}
	}
}

func (e *Engine) labelFor(procs []model.ProcStat, i int, byPID map[uint32]int) string {
	// 从自身沿父链上溯，chain[0]=自身，chain[len-1]=根。
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
		cur = procs[idx].PPID
		if cur == 0 {
			break
		}
	}
	// 从根向下找第一个识别规则命中。
	for j := len(chain) - 1; j >= 0; j-- {
		q := &procs[chain[j]]
		if label, ok := e.match(q.Name, q.Path); ok {
			return label
		}
	}
	if s, ok := e.sticky[procs[i].PID]; ok && s != "" {
		return s
	}
	if len(chain) > 0 {
		root := strings.ToLower(procs[chain[len(chain)-1]].Name)
		if r, ok := sysRoots[root]; ok {
			return r
		}
		return root
	}
	return strings.ToLower(procs[i].Name)
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
