package store

import (
	"fmt"
	"sort"
	"time"
)

// recoverPending 重建期间的挂起迁移（与 app.recordTransitions 同语义）。
type recoverPending struct {
	inc     Incident
	sinceMs int64
}

// MinDwellMs 状态迁移确认的最小驻留时长（毫秒）——采集端与重建端共用
// 同一语义，恢复出的事件与实时记录的口径一致。
const MinDwellMs = 30_000

// RecorderStall 记录器冻结判定阈值：相邻 tick 间隔超过它即视为一次
// 采样管线停摆（磁盘卡死把阻塞写挂住/系统冻结/睡眠——无论哪种，时间
// 线都必须诚实标注盲区，而不是让人误以为「当时一切正常」）。
// 必须保持 time.Duration 类型（曾因无类型常量 60_000 被当作 60 微秒、
// 每个 2s tick 都误判为冻结的单元事故）。
const RecorderStall = 60 * time.Second

// ScanGaps 扫描 [from,to] 的 totals 相邻 tick 间隔，返回超过
// RecorderStall 的断档（起点时刻与时长毫秒）。
func ScanGaps(dir string, from, to time.Time) (gaps []Incident, err error) {
	recs, _, err := LoadTotals(dir, from, to)
	if err != nil {
		return nil, err
	}
	var prev int64
	for _, r := range recs {
		if prev > 0 && time.Duration(r.T-prev)*time.Millisecond >= RecorderStall {
			gaps = append(gaps, Incident{
				T:    prev,
				Dim:  "recorder",
				From: "ok",
				To:   "stalled",
				Detail: fmt.Sprintf("记录器冻结 %.0f 分钟（%s ~ %s 期间数据缺失）",
					float64(r.T-prev)/60000,
					time.UnixMilli(prev).Format("15:04:05"),
					time.UnixMilli(r.T).Format("15:04:05")),
			})
		}
		prev = r.T
	}
	return gaps, nil
}

// RecoverIncidents 从 totals 的逐拍健康状态图（h 字段）重建 [from,to]
// 内缺失的事件，返回需要补写的事件列表。用于 incidents 静默失败事故
// （v0.5.0 及更早）后的数据恢复：totals 完好而 incidents 有缺口时，
// 时间线可以从持久化证据中重建。
//
// 语义：
//   - prevStatus 从 from 之前文件里最后一个事件续起（跨重启状态连续性，
//     比 loadIncidentBaseline 的 30 分钟新鲜度限制更完整）；
//   - 逐拍重放与 app.recordTransitions 相同的挂起/30s 驻留/跳档状态机
//     （时间戳用 totals 的真实 tick 时刻，冻结断档自然跨过）；
//   - 与文件中已有事件按 (t,dim,from,to) 精确去重——已写过的不再补。
//
// 注意：detail（均值等上下文）未持久化，重建事件为空 detail。
func RecoverIncidents(dir string, from, to time.Time) (recovered []Incident, err error) {
	history, err := LoadIncidents(dir, time.Unix(0, 0), to)
	if err != nil {
		return nil, err
	}
	// 去重索引 + prevStatus 种子（from 之前每维度最后状态）
	type key struct {
		t             int64
		dim, from, to string
	}
	skip := map[key]bool{}
	prevStatus := map[string]string{}
	for _, in := range history {
		skip[key{in.T, in.Dim, in.From, in.To}] = true
		if in.T < from.UnixMilli() {
			prevStatus[in.Dim] = in.To
		}
	}

	recs, _, err := LoadTotals(dir, from, to)
	if err != nil {
		return nil, err
	}

	pending := map[string]recoverPending{}
	for _, r := range recs {
		if r.Health == nil {
			continue
		}
		nowMs := r.T
		var confirmed []Incident
		for dim, st := range r.Health {
			if p, has := pending[dim]; has {
				switch {
				case st == p.inc.From:
					delete(pending, dim) // 抖动脉冲
				case st != p.inc.To:
					confirmed = append(confirmed, p.inc)
					pending[dim] = recoverPending{
						inc:     Incident{T: nowMs, Dim: dim, From: p.inc.To, To: st},
						sinceMs: nowMs,
					}
				case nowMs-p.sinceMs >= MinDwellMs:
					confirmed = append(confirmed, p.inc)
					delete(pending, dim)
					prevStatus[dim] = st
				}
				continue
			}
			if prev, had := prevStatus[dim]; had {
				if prev != st {
					pending[dim] = recoverPending{
						inc:     Incident{T: nowMs, Dim: dim, From: prev, To: st},
						sinceMs: nowMs,
					}
				}
				continue
			}
			prevStatus[dim] = st // 基线
		}
		for _, inc := range confirmed {
			if !skip[key{inc.T, inc.Dim, inc.From, inc.To}] {
				recovered = append(recovered, inc)
			}
		}
	}
	// 收尾：窗口结束时仍挂起的迁移一并补回（状态确实变化过，只是
	// 驻留尚未满 30s 就到了窗口边界）
	for _, p := range pending {
		if !skip[key{p.inc.T, p.inc.Dim, p.inc.From, p.inc.To}] {
			recovered = append(recovered, p.inc)
		}
	}
	sort.Slice(recovered, func(i, j int) bool { return recovered[i].T < recovered[j].T })
	return recovered, nil
}
