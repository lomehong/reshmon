//go:build windows

package syswin

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type memoryStatusEx struct {
	Length          uint32
	MemoryLoad      uint32
	TotalPhys       uint64
	AvailPhys       uint64
	TotalPageFile   uint64
	AvailPageFile   uint64
	TotalVirtual    uint64
	AvailVirtual    uint64
	AvailExtVirtual uint64
}

// performanceInformation 对应 psapi PERFORMANCE_INFORMATION。
type performanceInformation struct {
	Cb                uint32
	_                 uint32
	CommitTotal       uintptr
	CommitLimit       uintptr
	CommitPeak        uintptr
	PhysicalTotal     uintptr
	PhysicalAvailable uintptr
	SystemCache       uintptr
	KernelTotal       uintptr
	KernelPaged       uintptr
	KernelNonpaged    uintptr
	PageSize          uintptr
	HandleCount       uint32
	ProcessCount      uint32
	ThreadCount       uint32
}

// MemStatOS 系统级内存与系统负载摘要。
type MemStatOS struct {
	PhysTotalB   uint64
	PhysAvailB   uint64
	PhysLoadPct  float64
	CommitTotalB uint64
	CommitLimitB uint64
	HandleCount  uint64
	ProcessCount uint64
	ThreadCount  uint64
}

// ReadMemStatOS 读物理内存（GlobalMemoryStatusEx）与提交内存（GetPerformanceInfo）。
func ReadMemStatOS() (MemStatOS, error) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMemStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return MemStatOS{}, &PDHError{Op: "GlobalMemoryStatusEx", Code: 0}
	}
	out := MemStatOS{
		PhysTotalB:  ms.TotalPhys,
		PhysAvailB:  ms.AvailPhys,
		PhysLoadPct: float64(ms.MemoryLoad),
	}
	var pi performanceInformation
	pi.Cb = uint32(unsafe.Sizeof(pi))
	if r2, _, _ := procGetPerfInfo.Call(uintptr(unsafe.Pointer(&pi)), uintptr(pi.Cb)); r2 != 0 {
		page := uint64(pi.PageSize)
		out.CommitTotalB = uint64(pi.CommitTotal) * page
		out.CommitLimitB = uint64(pi.CommitLimit) * page
		out.HandleCount = uint64(pi.HandleCount)
		out.ProcessCount = uint64(pi.ProcessCount)
		out.ThreadCount = uint64(pi.ThreadCount)
	}
	return out, nil
}

// UptimeMs 系统启动至今的毫秒数。
func UptimeMs() uint64 {
	r, _, _ := procGetTickCount64.Call()
	return uint64(r)
}

var procGetActiveProcCount = modKernel32.NewProc("GetActiveProcessorCount")

// ActiveCPUCount 全机逻辑处理器数（跨全部处理器组）。
// runtime.NumCPU() 受进程默认组/亲和性约束，作 CPU% 分母会在受限时高估，
// 这里用 GetActiveProcessorCount(ALL_PROCESSOR_GROUPS) 对齐全机口径。
func ActiveCPUCount() int {
	const allGroups = 0xFFFF
	r, _, _ := procGetActiveProcCount.Call(allGroups)
	if n := int(r); n > 0 {
		return n
	}
	return runtime.NumCPU()
}

// PidAlive 判断进程是否仍存活（锁文件里的 PID 是否属于活着的实例）。
func PidAlive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// SetupConsole 让终端输出 UTF-8 并启用 ANSI 转义（VT）支持。
func SetupConsole() {
	_ = windows.SetConsoleOutputCP(65001)
	var mode uint32
	if h := windows.Handle(os.Stdout.Fd()); windows.GetConsoleMode(h, &mode) == nil {
		_ = windows.SetConsoleMode(h, mode|0x0004) // ENABLE_VIRTUAL_TERMINAL_PROCESSING
	}
}
