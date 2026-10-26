package report

import (
	"strings"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/store"
)

func TestTopWindowsSeparation(t *testing.T) {
	vals := make([]float64, 60)
	for i := range vals {
		vals[i] = 5
	}
	for i := 20; i < 30; i++ { // 单一高峰段
		vals[i] = 100
	}
	wins := topWindows(vals, 5, 3)
	if len(wins) == 0 {
		t.Fatal("未找到窗口")
	}
	first := wins[0]
	// 后续窗口不得与首个窗口（±一个窗宽）重叠
	for _, w := range wins[1:] {
		if w[0] < first[1]+5 && w[1] > first[0]-5 {
			t.Fatalf("窗口重叠: %v vs %v", first, w)
		}
	}
	// 首窗应落在高峰段
	if first[0] < 15 || first[1] > 35 {
		t.Fatalf("首窗未覆盖高峰: %v", first)
	}
}

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	w, err := store.NewWriter(dir, 1, 14)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 2, 9, 0, 0, 0, time.Local)
	w.SetNow(func() time.Time { return base })

	for i := 0; i < 40; i++ {
		cpu := 20.0
		if i >= 10 && i < 25 {
			cpu = 93 // 一段持续饱和
		}
		ts := base.Add(time.Duration(i) * 2 * time.Second)
		w.Append(model.Totals{
			T: ts.UnixMilli(), CPU: cpu, MemUsedPct: 50, CommitPct: 60,
			Agents: []model.AgentStat{{Name: "ZCode", CpuPct: cpu * 0.3, MemPrivB: 3e9}},
		})
	}
	w.AppendProcs(base.Add(30*time.Second), []model.ProcStat{{PID: 1, Name: "go.exe", Agent: "ZCode", CpuPct: 27}})
	w.Close()

	out, err := Run(dir, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CPU 峰值窗口", "ZCode", "go.exe", "Agent 汇总"} {
		if !strings.Contains(out, want) {
			t.Fatalf("报告缺少 %q:\n%s", want, out)
		}
	}
}

func TestRunEmpty(t *testing.T) {
	out, err := Run(t.TempDir(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "没有历史记录") {
		t.Fatalf("空数据提示错误: %s", out)
	}
}
