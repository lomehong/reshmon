//go:build windows

// Package syswin 封装本工具所需的 Windows 底层数据源：
// PDH 性能计数器（RAW 路线）、进程枚举、系统内存、DXGI 显示适配器与 NVML。
//
// 为什么用 RAW：PdhGetFormattedCounterArrayW 在部分系统上行为异常
// （NULL 探测返回 INVALID_DATA、给定缓冲时崩溃），而 RAW 数组
// PdhGetRawCounterArrayW 签名为 (hCounter, &size, &count, buf)，稳定可用。
// 速率一律由调用方对相邻两帧 RAW 值做差分计算。
package syswin

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modPDH           = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery = modPDH.NewProc("PdhOpenQuery")
	procPdhAddEng    = modPDH.NewProc("PdhAddEnglishCounterW")
	procPdhCollect   = modPDH.NewProc("PdhCollectQueryData")
	procPdhCloseQ    = modPDH.NewProc("PdhCloseQuery")
	procPdhRawArray  = modPDH.NewProc("PdhGetRawCounterArrayW")
	procPdhTimeBase  = modPDH.NewProc("PdhGetCounterTimeBase")
)

const (
	pdhMoreData    = 0x800007D2
	pdhNoData      = 0x800007D5
	pdhInvalidData = 0xC0000BC0
)

// PDHError 携带 PDH 原始返回码，便于区分"个别计数器暂无实例"等软错误。
type PDHError struct {
	Op   string
	Path string
	Code uint32
}

func (e *PDHError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s %s 失败 0x%08X", e.Op, e.Path, e.Code)
	}
	return fmt.Sprintf("%s 失败 0x%08X", e.Op, e.Code)
}

// IsPDHSoft 判断是否为可容忍的软错误（个别计数器暂无实例/暂无数据）。
func IsPDHSoft(err error) bool {
	var pe *PDHError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Code == pdhNoData || pe.Code == pdhInvalidData
}

// PDHQuery 一组性能计数器查询：Add 后反复 Collect。
type PDHQuery struct {
	handle   uintptr
	collects int
}

func PDHOpen() (*PDHQuery, error) {
	var h uintptr
	r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return nil, &PDHError{Op: "PdhOpenQuery", Code: uint32(r)}
	}
	return &PDHQuery{handle: h}, nil
}

func (q *PDHQuery) Close() {
	if q.handle != 0 {
		procPdhCloseQ.Call(q.handle)
		q.handle = 0
	}
}

// Add 添加英文计数器路径（不受系统语言影响），支持通配实例 (*)。
func (q *PDHQuery) Add(path string) (*PDHCounter, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var ch uintptr
	r, _, _ := procPdhAddEng.Call(q.handle, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&ch)))
	if r != 0 {
		return nil, &PDHError{Op: "添加计数器", Path: path, Code: uint32(r)}
	}
	return &PDHCounter{handle: ch, path: path}, nil
}

// Collect 采集查询内全部计数器。速率需相邻两帧 RAW 值做差分。
// 个别计数器暂无实例时 PDH 会整体返回错误，其余计数器数据仍可读取，
// 调用方用 IsPDHSoft 判定后继续。
func (q *PDHQuery) Collect() error {
	r, _, _ := procPdhCollect.Call(q.handle)
	q.collects++
	if r != 0 {
		return &PDHError{Op: "PdhCollectQueryData", Code: uint32(r)}
	}
	return nil
}

// Collects 已执行的采集次数。
func (q *PDHQuery) Collects() int { return q.collects }

type PDHCounter struct {
	handle uintptr
	path   string
}

// TimeBase 返回计数器的时间基准（每秒 tick 数）。
// PERF_AVERAGE_TIMER 类计数器必须用它换算；不可得时回退 1e7（QPC 10MHz，
// 绝大多数现代平台的实际值）。
func (c *PDHCounter) TimeBase() float64 {
	var base uint64
	r, _, _ := procPdhTimeBase.Call(c.handle, uintptr(unsafe.Pointer(&base)))
	if r != 0 || base == 0 {
		return 1e7
	}
	return float64(base)
}

// PDHRawItem 一个实例的原始计数器值。
// First/Second 语义随计数器类型不同（累计量/计时/计数），
// TS 为采集时间戳（100ns 单位，FILETIME）。
type PDHRawItem struct {
	Name   string
	First  int64
	Second int64
	TS     int64
}

type rawCounterItem struct {
	SzName  *uint16
	CStatus uint32
	TSLo    uint32
	TSHi    uint32
	_       uint32
	First   int64
	Second  int64
	Multi   uint32
	_2      uint32
}

// RawItems 返回通配计数器当前全部实例的原始值；CStatus 非零的实例被跳过。
func (c *PDHCounter) RawItems() ([]PDHRawItem, error) {
	isz := int(unsafe.Sizeof(rawCounterItem{})) // 48
	var size, count uint32
	r, _, _ := procPdhRawArray.Call(c.handle, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if r == 0 {
		return nil, nil
	}
	if r != pdhMoreData {
		return nil, &PDHError{Op: "读原始计数器", Path: c.path, Code: uint32(r)}
	}
	buf := make([]rawCounterItem, int(size)/isz+8)
	r2, _, _ := procPdhRawArray.Call(c.handle, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	runtime.KeepAlive(buf)
	if r2 != 0 {
		return nil, &PDHError{Op: "读原始计数器", Path: c.path, Code: uint32(r2)}
	}
	n := int(count)
	if n > len(buf) {
		n = len(buf)
	}
	out := make([]PDHRawItem, 0, n)
	for i := 0; i < n; i++ {
		if buf[i].CStatus != 0 || buf[i].SzName == nil {
			continue
		}
		out = append(out, PDHRawItem{
			Name:   windows.UTF16PtrToString(buf[i].SzName),
			First:  buf[i].First,
			Second: buf[i].Second,
			TS:     int64(uint64(buf[i].TSHi)<<32 | uint64(buf[i].TSLo)),
		})
	}
	return out, nil
}
