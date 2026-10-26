//go:build windows

package sampler

import (
	"testing"

	"github.com/lomehong/reshmon/internal/syswin"
)

func TestParseGPUEngineInstance(t *testing.T) {
	pid, luid, ok := ParseGPUEngineInstance("pid_12540_luid_0x00000000_0x00013C5E_phys_0_eng_0_engtype_3D")
	if !ok || pid != 12540 {
		t.Fatalf("pid 解析失败: pid=%d luid=%s ok=%v", pid, luid, ok)
	}
	if want := syswin.LUIDKey(0x00000000, 0x00013C5E); luid != want {
		t.Fatalf("luid 归一化失败: %s != %s", luid, want)
	}
	if _, _, ok := ParseGPUEngineInstance("luid_0x00000000_0x00013C5E_phys_0"); ok {
		t.Fatal("无 pid 实例不应匹配")
	}
	if _, _, ok := ParseGPUEngineInstance("junk"); ok {
		t.Fatal("垃圾输入不应匹配")
	}
}

func TestParseLUIDInstance(t *testing.T) {
	luid, ok := ParseLUIDInstance("pid_1_luid_0x00000000_0x00013C5E_phys_0")
	if !ok || luid != "0x00000000_0x00013c5e" {
		t.Fatalf("解析失败: %s %v", luid, ok)
	}
	if _, ok := ParseLUIDInstance("no-luid-here"); ok {
		t.Fatal("垃圾输入不应匹配")
	}
}

func TestRateHelpers(t *testing.T) {
	a := rawVal{first: 1000, second: 10, ts: 10000000}
	b := rawVal{first: 2000, second: 20, ts: 20000000}
	if v := timerPct(a, b); v < 0.0099 || v > 0.0101 { // Δ1000 / Δ1e7 ×100 = 0.01%
		t.Fatalf("timerPct 口径变化: %v", v)
	}
	if r := bulkRate(a, b); r < 999 || r > 1001 { // Δ1000 字节 / Δ1 秒 = 1000/s
		t.Fatalf("bulkRate 口径变化: %v", r)
	}
	if ms := avgTimerMs(a, b); ms < 0.0099 || ms > 0.0101 { // Δ1000×100ns/Δ10次 /1e4 = 0.01ms/次
		t.Fatalf("avgTimerMs 口径变化: %v", ms)
	}
	if v := timerPct(b, a); v != 0 { // 时间倒流防护
		t.Fatalf("倒流应返回 0: %v", v)
	}
}
