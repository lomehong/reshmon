package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Incident 一次健康状态迁移（如 磁盘 1 D: ok→sat）。
type Incident struct {
	T      int64  `json:"t"` // unix 毫秒
	Dim    string `json:"dim"`
	From   string `json:"from"`
	To     string `json:"to"`
	Detail string `json:"detail,omitempty"`
}

// IncidentLog 全局追加的事件日志（incidents.jsonl）。
type IncidentLog struct {
	mu sync.Mutex
	f  *os.File
}

func OpenIncidentLog(dir string) (*IncidentLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "incidents.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &IncidentLog{f: f}, nil
}

// Append 追加事件；nil 接收者与失败均静默（事件不阻塞主流程）。
func (l *IncidentLog) Append(i Incident) {
	if l == nil || l.f == nil {
		return
	}
	b, err := json.Marshal(i)
	if err != nil {
		return
	}
	l.mu.Lock()
	_, _ = l.f.Write(append(b, '\n'))
	l.mu.Unlock()
}

func (l *IncidentLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	l.mu.Unlock()
}

// LoadIncidents 读取 [from,to] 的事件，按时间升序。
func LoadIncidents(dir string, from, to time.Time) ([]Incident, error) {
	recs, err := readJSONL[Incident](filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		return nil, err
	}
	fm, tm := from.UnixMilli(), to.UnixMilli()
	out := make([]Incident, 0, len(recs))
	for _, r := range recs {
		if r.T >= fm && r.T <= tm {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}
