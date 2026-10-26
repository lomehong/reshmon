//go:build windows && amd64

package syswin

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 诊断用途：RESHMON_ETW_DIAG=1 时枚举 StartTrace 参数组合的错误码，
// 定位 0x57（ERROR_INVALID_PARAMETER）的来源。正常运行测试不执行。
func TestETWDiagStartTraceVariants(t *testing.T) {
	if os.Getenv("RESHMON_ETW_DIAG") == "" {
		t.Skip("需 RESHMON_ETW_DIAG=1")
	}
	type variant struct {
		name      string
		systemLog bool
		flags     uint32
		guid      windows.GUID
	}
	mustGUID := func(s string) windows.GUID {
		g, err := windows.GUIDFromString(s)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	variants := []variant{
		{"syslogger+sysguid+flags", true, flagProcess | flagDiskIO | flagDiskIOInit | flagNoSysConfig, mustGUID("{9e814aad-3204-11d2-9a82-006008a86939}")},
		{"syslogger+sysguid+noflags", true, 0, mustGUID("{9e814aad-3204-11d2-9a82-006008a86939}")},
		{"syslogger+zeroguid+flags", true, flagProcess | flagDiskIO | flagDiskIOInit, mustGUID("{00000000-0000-0000-0000-000000000000}")},
		{"plain+sysguid+flags", false, flagProcess | flagDiskIO | flagDiskIOInit, mustGUID("{9e814aad-3204-11d2-9a82-006008a86939}")},
	}
	for i, v := range variants {
		name, _ := windows.UTF16PtrFromString("reshmon-diag")
		propsBuf := make([]byte, 120+264)
		props := (*etwTraceProperties)(unsafe.Pointer(&propsBuf[0]))
		props.Wnode.BufferSize = uint32(len(propsBuf))
		props.Wnode.Flags = wnodeFlagTracedGuid
		props.Wnode.Guid = v.guid
		props.Wnode.ClientContext = 1
		props.LogFileMode = eventTraceRealTimeMode
		if v.systemLog {
			props.LogFileMode |= eventTraceSystemLogger
		}
		props.FlushTimer = 1
		props.BufferSize = 64
		props.EnableFlags = v.flags
		props.LoggerNameOffset = 120
		var session uint64
		r, _, _ := procStartTrace.Call(uintptr(unsafe.Pointer(&session)),
			uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(props)))
		t.Logf("variant %d %s -> 0x%X", i, v.name, uint32(r))
		if r == 0 {
			// 立即回收会话
			procControlTrace.Call(0, uintptr(unsafe.Pointer(name)),
				uintptr(unsafe.Pointer(props)), eventTraceControlStop)
		}
	}
}
