package model

import "testing"

func TestThermalText(t *testing.T) {
	cases := []struct {
		name string
		cpu  CPUStat
		want string
	}{
		{"全不可得（零值）", CPUStat{}, ""},
		{"全不可得（-1）", CPUStat{TempC: -1, PkgW: -1}, ""},
		{"零值一律视为不可得", CPUStat{TempC: 0, PkgW: 0, CoreW: 0, GpuW: 0}, ""},
		{"只有温度", CPUStat{TempC: 57.1}, "温度 57.1℃"},
		{"温度+封装", CPUStat{TempC: 57.1, PkgW: 45.6}, "温度 57.1℃ · 封装 45.6W"},
		{"温度+封装+核心+核显", CPUStat{TempC: 57.1, PkgW: 45.6, CoreW: 23.2, GpuW: 8.8},
			"温度 57.1℃ · 封装 45.6W（核心 23.2W · 核显 8.8W）"},
		{"只有封装+核显", CPUStat{PkgW: 45.6, GpuW: 8.8}, "封装 45.6W（核显 8.8W）"},
		{"封装有值而核心不可得", CPUStat{PkgW: 45.6, CoreW: 0, GpuW: 8.8}, "封装 45.6W（核显 8.8W）"},
		{"固定值标注", CPUStat{TempC: 5, ThermalStale: true}, "温度 5.0℃（读数未变化，可能为固定值）"},
		{"节流标注", CPUStat{TempC: 98, Throttling: true}, "温度 98.0℃（ACPI 报告节流）"},
	}
	for _, c := range cases {
		if got := c.cpu.ThermalText(); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestDiskKey(t *testing.T) {
	cases := map[string]string{
		"0 C:":    "0",
		"1 D: E:": "1",
		"2":       "2",
		"":        "",
	}
	for in, want := range cases {
		if got := DiskKey(in); got != want {
			t.Fatalf("DiskKey(%q) = %q want %q", in, got, want)
		}
	}
}
