// Package store 负责历史数据：内存环形缓冲（图表/判定用）与
// JSONL 落盘（回溯分析用），按天轮转、gzip 压缩、按保留天数清理。
package store

import (
	"sync"

	"github.com/lomehong/reshmon/internal/model"
)

// Ring 内存环形缓冲，容量单位为 tick 数（默认 3600 ≈ 2 小时 @2s）。
type Ring struct {
	mu   sync.RWMutex
	cap  int
	recs []model.Totals
}

func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{cap: capacity}
}

func (r *Ring) Push(t model.Totals) {
	r.mu.Lock()
	r.recs = append(r.recs, t)
	if len(r.recs) > r.cap {
		copy(r.recs, r.recs[len(r.recs)-r.cap:])
		r.recs = r.recs[:r.cap]
	}
	r.mu.Unlock()
}

// Last 返回最近 n 条拷贝（时间升序）。
func (r *Ring) Last(n int) []model.Totals {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n > len(r.recs) {
		n = len(r.recs)
	}
	out := make([]model.Totals, n)
	copy(out, r.recs[len(r.recs)-n:])
	return out
}

func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.recs)
}
