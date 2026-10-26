//go:build windows

package syswin

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modDXGI                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = modDXGI.NewProc("CreateDXGIFactory1")
)

// IID_IDXGIFactory1 {770aae78-f26f-4dba-a829-253c83d1b387}
var iidIDXGIFactory1 = windows.GUID{
	Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba,
	Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87},
}

const (
	dxgiErrNotFound = 0x887A0002
	vtblRelease     = 2
	vtblEnumAdapt1  = 12 // IDXGIFactory1::EnumAdapters1
	vtblGetDesc1    = 10 // IDXGIAdapter1::GetDesc1
)

// DXGIAdapter 一块显示适配器的静态信息。
type DXGIAdapter struct {
	Description    string
	LUIDKey        string // "0x%08x_0x%08x"（高_低），与 GPU 计数器实例的归一化 LUID 一致
	VendorID       uint32
	Vendor         string
	DedicatedVRAMB uint64
	SharedSysMemB  uint64
	IsWARP         bool // Microsoft Basic Render Driver：CPU 软件渲染回退，非物理 GPU
}

var dxgiVendors = map[uint32]string{
	0x10DE: "NVIDIA",
	0x1002: "AMD",
	0x1022: "AMD",
	0x8086: "Intel",
	0x15AD: "VMware",
	0x1AF4: "Red Hat",
	0x1234: "Bochs",
	0x1414: "Microsoft",
}

// dxgiAdapterDesc1 对应 DXGI_ADAPTER_DESC1（amd64 布局一致）。
type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uint64
	DedicatedSystemMemory uint64
	SharedSystemMemory    uint64
	LUIDLow               uint32
	LUIDHigh              int32
	Flags                 uint32
}

// EnumDXGIAdapters 枚举显示适配器，用于把 GPU 性能计数器的 LUID
// 映射为适配器名称与显存容量。失败时 GPU 数据仍可用 LUID 标识。
func EnumDXGIAdapters() ([]DXGIAdapter, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, fmt.Errorf("dxgi.dll 不可用: %w", err)
	}
	var factory unsafe.Pointer
	hr, _, _ := procCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory1)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != 0 || factory == nil {
		return nil, fmt.Errorf("CreateDXGIFactory1 失败 0x%08X", hr)
	}
	defer comRelease(factory)

	var out []DXGIAdapter
	for i := uintptr(0); i < 16; i++ {
		var adapter unsafe.Pointer
		hr2, _, _ := comCall(factory, vtblEnumAdapt1, i, uintptr(unsafe.Pointer(&adapter)))
		if hr2 == dxgiErrNotFound {
			break
		}
		if hr2 != 0 || adapter == nil {
			return out, fmt.Errorf("EnumAdapters1(%d) 失败 0x%08X", i, hr2)
		}
		var desc dxgiAdapterDesc1
		hr3, _, _ := comCall(adapter, vtblGetDesc1, uintptr(unsafe.Pointer(&desc)))
		comRelease(adapter)
		if hr3 != 0 {
			continue
		}
		descStr := windows.UTF16ToString(desc.Description[:])
		// WARP（软件回退适配器）不再静默跳过：带上标记返回，由采样层
		// 全链路剔除其引擎/显存口径——否则它偶发的引擎活动会凭空多出
		// 一张"GPU 0x0000…"幻影卡片。
		isWARP := descStr == "Microsoft Basic Render Driver"
		vendor := "其他"
		if v, ok := dxgiVendors[desc.VendorID]; ok {
			vendor = v
		}
		if isWARP {
			vendor = "Microsoft"
		}
		out = append(out, DXGIAdapter{
			Description:    descStr,
			LUIDKey:        LUIDKey(uint32(desc.LUIDHigh), desc.LUIDLow),
			VendorID:       desc.VendorID,
			Vendor:         vendor,
			DedicatedVRAMB: desc.DedicatedVideoMemory,
			SharedSysMemB:  desc.SharedSystemMemory,
			IsWARP:         isWARP,
		})
	}
	return out, nil
}

// LUIDKey 生成与性能计数器实例一致的归一化 LUID 键（高在前）。
func LUIDKey(high, low uint32) string {
	return fmt.Sprintf("0x%08x_0x%08x", high, low)
}

// comCall 调 COM 对象的 vtable 方法。obj 必须来自成功创建的 COM 接口指针；
// 全程持有 unsafe.Pointer（不做 uintptr 回转），符合 go vet unsafe 检查。
func comCall(obj unsafe.Pointer, method uintptr, args ...uintptr) (uintptr, uintptr, syscall.Errno) {
	vtbl := *(**[16]uintptr)(obj)
	fn := vtbl[method]
	callArgs := append([]uintptr{uintptr(obj)}, args...)
	return syscall.SyscallN(fn, callArgs...)
}

func comRelease(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, vtblRelease)
	}
}
