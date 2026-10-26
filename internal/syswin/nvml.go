//go:build windows

package syswin

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modNVML          = windows.NewLazySystemDLL("nvml.dll")
	procNvmlInit     = modNVML.NewProc("nvmlInit_v2")
	procNvmlShutdown = modNVML.NewProc("nvmlShutdown")
	procNvmlDevCount = modNVML.NewProc("nvmlDeviceGetCount_v2")
	procNvmlHandle   = modNVML.NewProc("nvmlDeviceGetHandleByIndex")
	procNvmlDevName  = modNVML.NewProc("nvmlDeviceGetName")
	procNvmlDevTemp  = modNVML.NewProc("nvmlDeviceGetTemperature")
)

// NVMLDevice NVIDIA GPU 的补充信息（PDH 已覆盖全厂商的利用率/显存，
// 这里只补充 PDH 拿不到的温度）。
type NVMLDevice struct {
	Name  string
	TempC int // 不可得为 -1（与 model.GPUStat 的约定一致）
}

// NVMLDevices 读取 NVIDIA 附加信息。驱动未带 nvml.dll（或非 NVIDIA）时返回 nil。
// 全程防御式调用：任何异常只导致返回 nil，绝不影响主流程。
func NVMLDevices() (out []NVMLDevice) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	if err := procNvmlInit.Find(); err != nil {
		return nil
	}
	if r, _, _ := procNvmlInit.Call(); r != 0 {
		return nil
	}
	defer procNvmlShutdown.Call()

	var count uint32
	if r, _, _ := procNvmlDevCount.Call(uintptr(unsafe.Pointer(&count))); r != 0 || count == 0 || count > 16 {
		return nil
	}
	for i := uint32(0); i < count; i++ {
		var h uintptr
		if r, _, _ := procNvmlHandle.Call(uintptr(i), uintptr(unsafe.Pointer(&h))); r != 0 {
			continue
		}
		var dev NVMLDevice
		dev.TempC = -1 // 先置「不可得」，读到真实温度再覆盖
		name := make([]byte, 128)
		if r, _, _ := procNvmlDevName.Call(h, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name))); r == 0 {
			dev.Name = cstr(name)
		}
		var t uint32
		if r, _, _ := procNvmlDevTemp.Call(h, 0 /*NVML_TEMPERATURE_GPU*/, uintptr(unsafe.Pointer(&t))); r == 0 && t < 200 {
			dev.TempC = int(t)
		}
		out = append(out, dev)
	}
	return out
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
