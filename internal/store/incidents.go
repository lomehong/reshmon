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
//
// 写入自愈：磁盘冻结期间挂在坏句柄上的写会失败（Windows 设备错误后
// 同一句柄持续失败），而 incidents 不像 totals 每天轮转换新句柄——
// 不自愈就会静默丢事件直到重启（v0.5.0 实测：11-09 15:58 起 24h
// 零写入，事件只活在内存里，重启即丢）。因此写失败即关句柄、限速
// 重开重试，并保留失败计数供上层可见化。
type IncidentLog struct {
	mu      sync.Mutex
	dir     string
	f       *os.File
	failed  int    // 连续失败次数（成功后清零）
	lastErr string // 最近一次失败原因
}

func OpenIncidentLog(dir string) (*IncidentLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &IncidentLog{dir: dir}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open 打开句柄并做尾部修复：上次冻结中断留下的残行（无换行结尾）
// 会让追加式写入接在残行之后（读取端整行解析失败被跳过）。尾部修复
// 需要 ReadAt——句柄必须 O_RDWR（O_WRONLY 上 ReadAt 恒被拒）。
func (l *IncidentLog) open() error {
	f, err := os.OpenFile(filepath.Join(l.dir, "incidents.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if st, serr := f.Stat(); serr == nil && st.Size() > 0 {
		b := make([]byte, 1)
		if _, rerr := f.ReadAt(b, st.Size()-1); rerr == nil && b[0] != '\n' {
			_, _ = f.Write([]byte{'\n'})
		}
	}
	l.f = f
	return nil
}

// Append 追加事件；写失败自愈：关坏句柄→限速重开→重试一次。
// 失败计数经 Failures() 暴露，绝不静默。
func (l *IncidentLog) Append(i Incident) {
	if l == nil {
		return
	}
	b, err := json.Marshal(i)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.write(b) {
		l.failed, l.lastErr = 0, ""
		return
	}
	l.failed++
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	// 失败后首次立即重试；此后每 ~180 次（约 6 分钟 @2s）试探一次，
	// 避免磁盘持续不可用时每个 tick 都做一次打开的系统调用风暴。
	if l.failed > 1 && (l.failed-1)%180 != 0 {
		return
	}
	if l.open() == nil {
		l.write(b)
	}
}

// write 写一行；返回是否完整落盘。
func (l *IncidentLog) write(b []byte) bool {
	if l.f == nil {
		if l.lastErr == "" {
			l.lastErr = "句柄未打开"
		}
		return false
	}
	n, err := l.f.Write(append(b, '\n'))
	if err != nil {
		l.lastErr = err.Error()
		return false
	}
	if n != len(b)+1 {
		l.lastErr = "短写"
		return false
	}
	return true
}

// Failures 连续失败计数与最近错误（上层可见化用；健康时返回 0）。
func (l *IncidentLog) Failures() (int, string) {
	if l == nil {
		return 0, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failed, l.lastErr
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
