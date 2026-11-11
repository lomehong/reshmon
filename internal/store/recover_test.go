package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

func writeRecoverTick(t *testing.T, dir, day string, offsetMs int64, status string) {
	t.Helper()
	tt := model.Totals{
		T:      offsetMs,
		CPU:    50,
		Health: map[string]string{"overall": status, "cpu": status, "mem": "ok", "commit": "warn"},
	}
	b, err := json.Marshal(tt)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(totalsPath(dir, day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// 从 totals 的 h 图重建缺失事件：基线种子取自文件历史、脉冲丢弃、
// 驻留确认、跳档确认、已写事件精确去重。
func TestRecoverIncidents(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)

	// 文件历史：一条在窗口内的已有事件（10:01 cpu ok→warn，重放会精确
	// 命中去重）；一条窗口前的 commit 事件（种子 prevStatus[commit]=warn）
	l, err := OpenIncidentLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(Incident{T: base.Add(time.Minute).UnixMilli(), Dim: "cpu", From: "ok", To: "warn"})
	l.Append(Incident{T: base.Add(-2 * time.Minute).UnixMilli(), Dim: "commit", From: "ok", To: "warn"})
	l.Close()

	day := dayKey(base)
	writeRecoverTick(t, dir, day, base.UnixMilli(), "ok")                      // 基线：cpu/overall=ok
	writeRecoverTick(t, dir, day, base.Add(time.Minute).UnixMilli(), "warn")   // 挂起 ok→warn
	writeRecoverTick(t, dir, day, base.Add(2*time.Minute).UnixMilli(), "warn") // 驻留 60s → 确认
	writeRecoverTick(t, dir, day, base.Add(3*time.Minute).UnixMilli(), "sat")  // 跳档：warn→sat 挂起
	writeRecoverTick(t, dir, day, base.Add(4*time.Minute).UnixMilli(), "sat")  // 确认
	writeRecoverTick(t, dir, day, base.Add(5*time.Minute).UnixMilli(), "warn") // sat→warn 挂起
	writeRecoverTick(t, dir, day, base.Add(6*time.Minute).UnixMilli(), "warn") // 确认
	writeRecoverTick(t, dir, day, base.Add(7*time.Minute).UnixMilli(), "ok")   // warn→ok 挂起
	writeRecoverTick(t, dir, day, base.Add(8*time.Minute).UnixMilli(), "warn") // 脉冲：回原态丢弃

	recovered, err := RecoverIncidents(dir, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// cpu: 已有事件去重；overall: 全部补回；commit: 全程 warn 无事件
	type ev struct {
		dim, from, to string
		min           int
	}
	// 同一 tick 的 cpu/overall 两条事件顺序不确定（map 迭代序），按 (T,dim) 归一后比较
	sortSlice(recovered, func(i, j int) bool {
		if recovered[i].T != recovered[j].T {
			return recovered[i].T < recovered[j].T
		}
		return recovered[i].Dim < recovered[j].Dim
	})
	want := []ev{
		{"overall", "ok", "warn", 1},
		{"cpu", "warn", "sat", 3},
		{"overall", "warn", "sat", 3},
		{"cpu", "sat", "warn", 5},
		{"overall", "sat", "warn", 5},
	}
	if len(recovered) != len(want) {
		t.Fatalf("应恢复 %d 条，得到 %d: %+v", len(want), len(recovered), recovered)
	}
	for i, w := range want {
		got := recovered[i]
		mm := int(time.UnixMilli(got.T).Sub(base).Minutes() + 0.02)
		if got.Dim != w.dim || got.From != w.from || got.To != w.to || mm != w.min {
			t.Fatalf("第 %d 条不符: got %+v want %+v", i, got, w)
		}
	}
}

func sortSlice(s []Incident, less func(i, j int) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// 断档扫描：相邻 tick 间隔超阈值 → 生成 recorder 冻结事件（含时长与
// 起止时刻）；短间隔不误报。
func TestScanGaps(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	day := dayKey(base)
	writeRecoverTick(t, dir, day, base.UnixMilli(), "ok")
	writeRecoverTick(t, dir, day, base.Add(2*time.Second).UnixMilli(), "ok")
	writeRecoverTick(t, dir, day, base.Add(10*time.Minute).UnixMilli(), "ok") // 冻结 9 分 58 秒后恢复
	writeRecoverTick(t, dir, day, base.Add(10*time.Minute+2*time.Second).UnixMilli(), "ok")

	gaps, err := ScanGaps(dir, base, base.Add(time.Hour))
	if err != nil || len(gaps) != 1 {
		t.Fatalf("应发现 1 次冻结: %+v %v", gaps, err)
	}
	g := gaps[0]
	if g.Dim != "recorder" || g.From != "ok" || g.To != "stalled" {
		t.Fatalf("冻结事件字段错误: %+v", g)
	}
	if g.T != base.Add(2*time.Second).UnixMilli() {
		t.Fatalf("冻结起点应为最后一条正常 tick: %d", g.T)
	}
	if !strings.Contains(g.Detail, "10 分钟") || !strings.Contains(g.Detail, "10:00:02") {
		t.Fatalf("detail 应含冻结时长与起止: %s", g.Detail)
	}
	// 无断档 → 不误报（2s 正常间隔）
	dir2 := t.TempDir()
	day2 := dayKey(base)
	writeRecoverTick(t, dir2, day2, base.UnixMilli(), "ok")
	writeRecoverTick(t, dir2, day2, base.Add(2*time.Second).UnixMilli(), "ok")
	gaps2, err := ScanGaps(dir2, base, base.Add(time.Hour))
	if err != nil || len(gaps2) != 0 {
		t.Fatalf("无断档不应报: %+v %v", gaps2, err)
	}
}
func TestRecoverIncidentsIdempotent(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	day := dayKey(base)
	writeRecoverTick(t, dir, day, base.UnixMilli(), "ok")
	writeRecoverTick(t, dir, day, base.Add(time.Minute).UnixMilli(), "warn")
	writeRecoverTick(t, dir, day, base.Add(2*time.Minute).UnixMilli(), "warn")

	first, err := RecoverIncidents(dir, base, base.Add(time.Hour))
	if err != nil || len(first) != 2 { // overall+cpu 各一条 ok→warn
		t.Fatalf("首次应恢复 2 条: %+v %v", first, err)
	}
	// 写回文件（模拟 cmdRecover 的落盘）
	l, err := OpenIncidentLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range first {
		l.Append(in)
	}
	l.Close()

	second, err := RecoverIncidents(dir, base, base.Add(time.Hour))
	if err != nil || len(second) != 0 {
		t.Fatalf("重跑应零新增: %+v %v", second, err)
	}
}
