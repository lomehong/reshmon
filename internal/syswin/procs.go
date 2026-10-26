//go:build windows

package syswin

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modKernel32           = windows.NewLazySystemDLL("kernel32.dll")
	modPSAPI              = windows.NewLazySystemDLL("psapi.dll")
	procGetPMI            = modPSAPI.NewProc("GetProcessMemoryInfo")
	procGetPerfInfo       = modPSAPI.NewProc("GetPerformanceInfo")
	procGetIoCounters     = modKernel32.NewProc("GetProcessIoCounters")
	procGetTickCount64    = modKernel32.NewProc("GetTickCount64")
	procGlobalMemStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
)

// ProcInfo 一次进程枚举得到的原始累计值；差分（速率）在采样层完成。
type ProcInfo struct {
	PID, PPID   uint32
	Name        string
	Path        string
	Create      uint64 // 进程创建时间（FILETIME 100ns）——PID 复用防护的复合键
	CPU100ns    uint64 // kernel+user 累计（100ns 单位）
	WorkingSetB uint64
	PrivateB    uint64
	ReadB       uint64 // IO 读累计字节数（含文件/网络/设备）
	WriteB      uint64
}

// processMemoryCountersEx 对应 PROCESS_MEMORY_COUNTERS_EX（x/sys 未提供 EX 版）。
type processMemoryCountersEx struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

// EnumProcs 枚举全部进程并抓取每进程 CPU/内存/IO 原始累计值。
// 受保护进程的部分字段可能缺失（零值），属正常现象。
func EnumProcs() ([]ProcInfo, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("进程快照失败: %w", err)
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return nil, fmt.Errorf("进程枚举失败: %w", err)
	}
	out := make([]ProcInfo, 0, 256)
	for {
		pi := ProcInfo{
			PID:  pe.ProcessID,
			PPID: pe.ParentProcessID,
			Name: windows.UTF16ToString(pe.ExeFile[:]),
		}
		fillProcDetail(&pi)
		out = append(out, pi)
		if err := windows.Process32Next(snap, &pe); err != nil {
			break
		}
	}
	return out, nil
}

func fillProcDetail(pi *ProcInfo) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pi.PID)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)

	var create, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &create, &exit, &kernel, &user); err == nil {
		pi.Create = filetime100(create)
		pi.CPU100ns = filetime100(kernel) + filetime100(user)
	}

	var mc processMemoryCountersEx
	mc.CB = uint32(unsafe.Sizeof(mc))
	if r, _, _ := procGetPMI.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.CB)); r != 0 {
		pi.WorkingSetB = uint64(mc.WorkingSetSize)
		pi.PrivateB = uint64(mc.PrivateUsage)
	}

	var io windows.IO_COUNTERS
	if r, _, _ := procGetIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
		pi.ReadB = io.ReadTransferCount
		pi.WriteB = io.WriteTransferCount
	}

	n := uint32(1024)
	buf := make([]uint16, n)
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
		pi.Path = windows.UTF16ToString(buf[:n])
	}
}

func filetime100(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}
