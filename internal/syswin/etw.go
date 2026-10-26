//go:build windows && amd64

// ETW 短命进程 IO 追踪：内核会话（System Trace Provider）订阅 DiskIO +
// Process 事件，把"存活不足一个采样间隔"的进程 IO 补进归因账本。
//
// 事件口径（WMICore.mof，字段偏移跨版本稳定）：
//   - DiskIo Init（type 12/13，DiskIo_TypeGroup2）：发起线程上下文，
//     只含 Irp 指针（x64 8 字节）——记录 Irp→PID；
//   - DiskIo 完成（type 10/11，DiskIo_TypeGroup1）：含 TransferSize@8 与
//     Irp@32——按 Irp 回填发起进程的字节数（完成事件的 header PID 常是
//     DPC/System 上下文，不能直接用）；
//   - Process_V2（version≥3，type 1/2/3/4）：Start/End/DCStart/DCEnd，
//     布局 {EPROCESS 指针, ProcessId@8, ParentId@12, SessionId@16,
//     ExitStatus@20, SID@24(8+4n), ImageFileName(ANSI)}。
//
// 需要管理员权限（启动系统日志器会话 + 实时消费）；失败时调用方降级。
package syswin

import (
	"encoding/binary"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modAdvapi        = windows.NewLazySystemDLL("advapi32.dll")
	procStartTrace   = modAdvapi.NewProc("StartTraceW")
	procControlTrace = modAdvapi.NewProc("ControlTraceW")
	procOpenTrace    = modAdvapi.NewProc("OpenTraceW")
	procProcessTrace = modAdvapi.NewProc("ProcessTrace")
	procCloseTrace   = modAdvapi.NewProc("CloseTrace")
)

const (
	wnodeFlagTracedGuid       = 0x00020000
	eventTraceRealTimeMode    = 0x00000100
	eventTraceSystemLogger    = 0x02000000
	flagProcess               = 0x00000001
	flagDiskIO                = 0x00000100
	flagDiskIOInit            = 0x00000400
	flagNoSysConfig           = 0x10000000
	processTraceModeRealTime  = 0x00000100
	processTraceModeEventRec  = 0x10000000
	eventTraceControlStop     = 1
	errAlreadyExists          = 183
	etwSessionName            = "reshmon"
	etwMaxIrpEntries          = 1 << 16
	etwMaxTrackedProcs        = 4096
	diskEvtRead, diskEvtWrite = 10, 11
	diskEvtReadInit           = 12
	diskEvtWriteInit          = 13
	procEvtStart              = 1
	procEvtEnd                = 2
	procEvtDCStart            = 3
	procEvtDCEnd              = 4
)

var (
	guidDiskIO = windows.GUID{Data1: 0x3d6fa8d4, Data2: 0xfe05, Data3: 0x11d0,
		Data4: [8]byte{0x9d, 0xda, 0x00, 0xc0, 0x4f, 0xd7, 0xba, 0x7c}}
	guidProcess = windows.GUID{Data1: 0x3d6fa8d0, Data2: 0xfe05, Data3: 0x11d0,
		Data4: [8]byte{0x9d, 0xda, 0x00, 0xc0, 0x4f, 0xd7, 0xba, 0x7c}}
)

// EphemeralProc 一个已退出（或采样枚举不可见）进程的本窗未结算 IO。
type EphemeralProc struct {
	PID, PPID     uint32
	Name          string
	Create        uint64 // 进程启动时间戳（FILETIME 100ns）
	ReadB, WriteB uint64
}

// ---- FFI 结构体（amd64 布局，与 evntrace.h/evntcons.h 对齐；编译期断言钉死） ----

type etwWnodeHeader struct {
	BufferSize        uint32
	ProviderId        uint32
	HistoricalContext uint64
	TimeStamp         int64 // union KernelHandle/TimeStamp
	Guid              windows.GUID
	ClientContext     uint32
	Flags             uint32
}

type etwEventDescriptor struct {
	Id      uint16
	Version uint8
	Channel uint8
	Level   uint8
	Opcode  uint8
	Task    uint16
	Keyword uint64 // Task 结束于 8，天然 8 对齐，无 pad
}

type etwEventHeader struct {
	Size            uint16
	HeaderType      uint16
	Flags           uint16
	EventProperty   uint16
	ThreadId        uint32
	ProcessId       uint32
	TimeStamp       int64
	ProviderId      windows.GUID
	EventDescriptor etwEventDescriptor
	KernelTime      uint32
	UserTime        uint32
	ActivityId      windows.GUID
}

type etwEventRecord struct {
	EventHeader       etwEventHeader
	BufferContext     [2]byte
	ExtendedDataCount uint16
	UserDataLength    uint16
	ExtendedData      uintptr
	UserData          uintptr
	UserContext       uintptr
}

type etwTraceProperties struct {
	Wnode               etwWnodeHeader
	BufferSize          uint32
	MinimumBuffers      uint32
	MaximumBuffers      uint32
	MaximumFileSize     uint32
	LogFileMode         uint32
	FlushTimer          uint32
	EnableFlags         uint32
	AgeLimit            uint32
	NumberOfBuffers     uint32
	FreeBuffers         uint32
	EventsLost          uint32
	BuffersWritten      uint32
	LogBuffersLost      uint32
	RealTimeBuffersLost uint32
	LoggerThreadId      int64
	LogFileNameOffset   uint32
	LoggerNameOffset    uint32
}

type etwTimeZoneInfo struct { // TIME_ZONE_INFORMATION，172 字节
	Bias         int32
	StandardName [32]uint16
	StandardDate [8]uint16
	StandardBias int32
	DaylightName [32]uint16
	DaylightDate [8]uint16
	DaylightBias int32
}

type etwTraceHeader struct { // EVENT_TRACE（88B），仅用于 EVENT_TRACE_LOGFILE 布局占位
	HeaderSize       uint16
	FieldTypeFlags   uint16
	Version          uint32
	ThreadId         uint32
	ProcessId        uint32
	TimeStamp        int64
	Guid             windows.GUID
	KernelTime       uint32
	UserTime         uint32
	InstanceId       uint32
	ParentInstanceId uint32
	ParentGuid       windows.GUID
	MofData          uintptr
	MofLength        uint32
	_                uint32 // ClientContext/BufferContext 联合体占位
}

type etwLogFileHeader struct { // TRACE_LOGFILE_HEADER，280 字节
	BufferSize         uint32
	Version            uint32
	ProviderVersion    uint32
	NumberOfProcessors uint32
	EndTime            int64
	TimerResolution    uint32
	MaximumFileSize    uint32
	LogFileMode        uint32
	BuffersWritten     uint32
	LogInstanceGuid    windows.GUID
	LoggerName         uintptr
	LogFileName        uintptr
	TimeZone           etwTimeZoneInfo
	_                  uint32 // 对齐：TimeZone 172B 后 BootTime 需 8 对齐
	BootTime           int64
	PerfFreq           int64
	StartTime          int64
	ReservedFlags      uint32
	BuffersLost        uint32
}

type etwLogfileW struct { // EVENT_TRACE_LOGFILEW
	LogFileName         uintptr
	LoggerName          uintptr
	CurrentTime         int64
	BuffersRead         uint32
	ProcessTraceMode    uint32
	CurrentEvent        etwTraceHeader   // EVENT_TRACE（88B，此处仅保布局）
	LogfileHeader       etwLogFileHeader // TRACE_LOGFILE_HEADER（280B）
	BufferCallback      uintptr
	BufferSize          uint32
	Filled              uint32
	EventsLost          uint32
	_                   uint32
	EventRecordCallback uintptr
	IsKernelTrace       uint32
	_                   uint32
	Context             uintptr
}

// 布局断言：任何字段增删导致布局漂移都在编译期暴露（上下两条配合，
// 尺寸必须恰好相等，否则出现负数组长度编译错误）。
var (
	_ [48 - unsafe.Sizeof(etwWnodeHeader{})]byte
	_ [unsafe.Sizeof(etwWnodeHeader{}) - 48]byte
	_ [16 - unsafe.Sizeof(etwEventDescriptor{})]byte
	_ [unsafe.Sizeof(etwEventDescriptor{}) - 16]byte
	_ [80 - unsafe.Sizeof(etwEventHeader{})]byte
	_ [unsafe.Sizeof(etwEventHeader{}) - 80]byte
	_ [112 - unsafe.Sizeof(etwEventRecord{})]byte
	_ [unsafe.Sizeof(etwEventRecord{}) - 112]byte
	_ [120 - unsafe.Sizeof(etwTraceProperties{})]byte
	_ [unsafe.Sizeof(etwTraceProperties{}) - 120]byte
	_ [88 - unsafe.Sizeof(etwTraceHeader{})]byte
	_ [unsafe.Sizeof(etwTraceHeader{}) - 88]byte
	_ [280 - unsafe.Sizeof(etwLogFileHeader{})]byte
	_ [unsafe.Sizeof(etwLogFileHeader{}) - 280]byte
)

// ---- 纯逻辑状态机（与 syscall 解耦，可单测） ----

type etwProcEntry struct {
	pid, ppid         uint32
	name              string
	create            uint64
	winRead, winWrite uint64 // 自上次 Consume 以来的字节
	dead              bool
}

type etwIrpEnt struct {
	pid uint32
	seq uint64
}

// etwState 事件累加器：DiskIO Init 记 Irp→PID，完成事件按 Irp 归账；
// Process Start/End 维护进程身份（PID 复用时按启动时间戳换代）。
type etwState struct {
	mu          sync.Mutex
	procs       map[uint32]*etwProcEntry
	dead        []EphemeralProc // 已退出且尚有未结算字节的进程，等 TakeDead 取走
	irps        map[uintptr]*etwIrpEnt
	irpSeq      uint64
	irpOld      []uintptr // FIFO 驱逐序
	unmatchedIO uint64
}

func newEtwState() *etwState {
	return &etwState{procs: map[uint32]*etwProcEntry{}, irps: map[uintptr]*etwIrpEnt{}}
}

func (st *etwState) trimProcs() {
	if len(st.procs) <= etwMaxTrackedProcs {
		return
	}
	var oldest uint32
	var oldestTS uint64
	first := true
	for pid, e := range st.procs {
		if first || e.create < oldestTS {
			oldest, oldestTS, first = pid, e.create, false
		}
	}
	if e := st.procs[oldest]; e != nil && (e.winRead > 0 || e.winWrite > 0) {
		st.finalize(e)
	}
	delete(st.procs, oldest)
}

func (st *etwState) finalize(e *etwProcEntry) {
	if e.winRead == 0 && e.winWrite == 0 {
		return
	}
	st.dead = append(st.dead, EphemeralProc{
		PID: e.pid, PPID: e.ppid, Name: e.name, Create: e.create,
		ReadB: e.winRead, WriteB: e.winWrite,
	})
	e.winRead, e.winWrite = 0, 0
}

// onDiskInit DiskIo ReadInit/WriteInit：UserData = {Irp ptr@0}（TypeGroup2，
// IssuingThreadId 其后，不用）。发起线程上下文 → header PID 即归因对象。
func (st *etwState) onDiskInit(pid uint32, data []byte) {
	if len(data) < 8 {
		return
	}
	irp := uintptr(binary.LittleEndian.Uint64(data[0:8]))
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.irps) >= etwMaxIrpEntries && st.irps[irp] == nil {
		oldest := st.irpOld[0]
		st.irpOld = st.irpOld[1:]
		delete(st.irps, oldest)
	}
	st.irpSeq++
	if st.irps[irp] == nil {
		st.irpOld = append(st.irpOld, irp)
	}
	st.irps[irp] = &etwIrpEnt{pid: pid, seq: st.irpSeq}
}

// onDiskDone DiskIo Read/Write 完成：UserData = {DiskNumber@0, IrpFlags@4,
// TransferSize@8, Reserved@12, ByteOffset@16, FileObject ptr@24, Irp ptr@32}
// （TypeGroup1，尾部新增字段不影响前缀偏移）。
func (st *etwState) onDiskDone(headerPid uint32, evtType int, data []byte) {
	if len(data) < 36 {
		return
	}
	size := uint64(binary.LittleEndian.Uint32(data[8:12]))
	if size == 0 {
		return
	}
	irp := uintptr(binary.LittleEndian.Uint64(data[32:40]))
	st.mu.Lock()
	defer st.mu.Unlock()
	pid := headerPid
	if ent := st.irps[irp]; ent != nil {
		pid = ent.pid
		delete(st.irps, irp) // IRP 一次性配对
	} else {
		st.unmatchedIO += size
	}
	e := st.procEntryLocked(pid, 0)
	if evtType == diskEvtRead {
		e.winRead += size
	} else {
		e.winWrite += size
	}
}

// procEntryLocked 创建/获取进程条目（须持锁）。create==0 表示时间戳未知。
func (st *etwState) procEntryLocked(pid uint32, create uint64) *etwProcEntry {
	e := st.procs[pid]
	if e == nil {
		e = &etwProcEntry{pid: pid, create: create}
		st.procs[pid] = e
		st.trimProcs()
	}
	return e
}

// onProcessStart Process_V2(type 1/3)。UserData = {EPROCESS ptr@0, PID@8,
// PPID@12, SessionId@16, ExitStatus@20, SID@24(8+4n), ImageFileName(ANSI)}。
// 会话启动时的 DCStart 例举在册进程——顺带补全名字库。
func (st *etwState) onProcessStart(ts int64, data []byte) {
	if len(data) < 28 {
		return
	}
	pid := binary.LittleEndian.Uint32(data[8:12])
	ppid := binary.LittleEndian.Uint32(data[12:16])
	name := processImageName(data[24:])
	st.mu.Lock()
	defer st.mu.Unlock()
	if old := st.procs[pid]; old != nil && uint64(ts) > old.create && (old.winRead > 0 || old.winWrite > 0) {
		// PID 复用：旧身份的未结算字节先落账
		st.finalize(old)
	}
	e := st.procEntryLocked(pid, uint64(ts))
	e.ppid, e.name, e.dead = ppid, name, false
}

func (st *etwState) onProcessEnd(pid uint32) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := st.procs[pid]; e != nil {
		e.dead = true
	}
}

// processImageName 解析 SID 之后 4 对齐处的 ANSI 镜像名。
// SID 长度 = 8 + 4×SubAuthorityCount；解析失败返回空串（归因仍可走父链）。
func processImageName(rest []byte) string {
	if len(rest) < 2 {
		return ""
	}
	n := int(rest[1]) // SubAuthorityCount
	sidLen := 8 + 4*n
	if sidLen > len(rest) {
		return ""
	}
	off := (sidLen + 3) &^ 3
	if off >= len(rest) {
		return ""
	}
	end := off
	for end < len(rest) && rest[end] != 0 {
		end++
	}
	name := string(rest[off:end])
	if !printableASCII(name) {
		return ""
	}
	return name
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return len(s) > 0
}

// Consume 取走 pid 自上次结算以来的 IO 字节并清零（进程继续跟踪）。
func (st *etwState) Consume(pid uint32) (read, write uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	e := st.procs[pid]
	if e == nil {
		return 0, 0
	}
	read, write = e.winRead, e.winWrite
	e.winRead, e.winWrite = 0, 0
	return read, write
}

// TakeDead 结算已不存活于当前枚举中的条目：未结算字节转为一次性归因记录。
// alive 为本次采样枚举到的全部 PID。
func (st *etwState) TakeDead(alive map[uint32]bool) []EphemeralProc {
	st.mu.Lock()
	defer st.mu.Unlock()
	for pid, e := range st.procs {
		if !alive[pid] {
			st.finalize(e) // 有剩余字节才入队
			delete(st.procs, pid)
		}
	}
	out := st.dead
	st.dead = nil
	return out
}

// ---- 会话生命周期（syscall 薄层） ----

// EphemeralTracker 一个运行中的 ETW 会话 + 实时消费者。
type EphemeralTracker struct {
	state        *etwState
	sessionProps []byte // props + LoggerName 缓冲（Stop 时复用）
	sessionOK    bool
	traceHandle  uint64
	callback     uintptr
	stopped      chan struct{}
}

// StartEphemeralTracker 启动内核 ETW 会话（"reshmon"，DiskIO+Process）并
// 开始实时消费。需要管理员权限；错误时调用方应降级继续运行。
func StartEphemeralTracker() (*EphemeralTracker, error) {
	name, _ := windows.UTF16PtrFromString(etwSessionName)

	propsBuf := make([]byte, 120+264) // struct(120) + LoggerName 缓冲
	props := (*etwTraceProperties)(unsafe.Pointer(&propsBuf[0]))
	props.Wnode.BufferSize = uint32(len(propsBuf))
	props.Wnode.Flags = wnodeFlagTracedGuid
	// 自定义命名的 system logger（SYSTEM_LOGGER_MODE）不能用
	// SystemTraceControlGuid（保留给 NT Kernel Logger，会得 0x57）；
	// 置零 GUID 是该模式的正确姿势（实测非管理员时直达权限检查 0x5）。
	props.Wnode.ClientContext = 1 // QPC；消费端默认换算回系统时间
	props.LogFileMode = eventTraceRealTimeMode | eventTraceSystemLogger
	props.FlushTimer = 1
	props.BufferSize = 64 // KB
	props.EnableFlags = flagProcess | flagDiskIO | flagDiskIOInit | flagNoSysConfig
	props.LoggerNameOffset = 120
	props.LogFileNameOffset = 0 // 仅实时，不落 .etl 文件

	var session uint64
	r, _, _ := procStartTrace.Call(uintptr(unsafe.Pointer(&session)),
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(props)))
	if uint32(r) == errAlreadyExists {
		// 残留会话（上次异常退出）：先停再试一次
		procControlTrace.Call(0, uintptr(unsafe.Pointer(name)),
			uintptr(unsafe.Pointer(props)), eventTraceControlStop)
		r, _, _ = procStartTrace.Call(uintptr(unsafe.Pointer(&session)),
			uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(props)))
	}
	if r != 0 {
		if uint32(r) == 5 { // ERROR_ACCESS_DENIED
			return nil, fmt.Errorf("需要管理员权限（系统日志器会话被拒绝）")
		}
		return nil, fmt.Errorf("启动 ETW 会话失败 0x%X", uint32(r))
	}

	t := &EphemeralTracker{
		state:        newEtwState(),
		sessionProps: propsBuf,
		sessionOK:    true,
		stopped:      make(chan struct{}),
	}
	t.callback = windows.NewCallback(t.onEvent)

	lfBuf := etwLogfileW{
		LoggerName:          uintptr(unsafe.Pointer(name)),
		ProcessTraceMode:    processTraceModeRealTime | processTraceModeEventRec,
		EventRecordCallback: t.callback,
		Context:             uintptr(unsafe.Pointer(t)),
	}
	h, _, _ := procOpenTrace.Call(uintptr(unsafe.Pointer(&lfBuf)))
	t.traceHandle = uint64(h)
	if t.traceHandle == 0xFFFFFFFFFFFFFFFF {
		t.stopSession()
		return nil, fmt.Errorf("OpenTrace 失败（实时消费同样需要管理员权限）")
	}

	go func() {
		defer close(t.stopped)
		arr := [1]uint64{t.traceHandle}
		procProcessTrace.Call(uintptr(unsafe.Pointer(&arr[0])), 1, 0, 0)
	}()
	return t, nil
}

// onEvent EVENT_RECORD_CALLBACK：按 Provider GUID 分发，解析后交给状态机。
func (t *EphemeralTracker) onEvent(rec uintptr) uintptr {
	r := (*etwEventRecord)(unsafe.Pointer(rec))
	if r.UserData == 0 || r.UserDataLength == 0 {
		return 0
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(r.UserData)), int(r.UserDataLength))
	switch r.EventHeader.ProviderId {
	case guidDiskIO:
		switch r.EventHeader.EventDescriptor.Id {
		case diskEvtReadInit, diskEvtWriteInit:
			t.state.onDiskInit(r.EventHeader.ProcessId, data)
		case diskEvtRead, diskEvtWrite:
			t.state.onDiskDone(r.EventHeader.ProcessId, int(r.EventHeader.EventDescriptor.Id), data)
		}
	case guidProcess:
		if r.EventHeader.EventDescriptor.Version < 3 {
			return 0 // 旧版布局不支持（Win10+ 均 ≥3）
		}
		switch r.EventHeader.EventDescriptor.Id {
		case procEvtStart, procEvtDCStart:
			t.state.onProcessStart(r.EventHeader.TimeStamp, data)
		case procEvtEnd, procEvtDCEnd:
			if len(data) >= 12 {
				t.state.onProcessEnd(binary.LittleEndian.Uint32(data[8:12]))
			}
		}
	}
	return 0
}

// Consume 结算某 PID 本窗 IO 字节（存活进程防双计：常规差分已覆盖时调用
// 并丢弃返回值；首拍/受保护进程则把返回值并入速率）。
func (t *EphemeralTracker) Consume(pid uint32) (read, write uint64) {
	return t.state.Consume(pid)
}

// TakeDead 取出已退出进程的一次性归因记录（alive=当前枚举 PID 集）。
func (t *EphemeralTracker) TakeDead(alive map[uint32]bool) []EphemeralProc {
	return t.state.TakeDead(alive)
}

func (t *EphemeralTracker) stopSession() {
	if !t.sessionOK {
		return
	}
	t.sessionOK = false
	name, _ := windows.UTF16PtrFromString(etwSessionName)
	props := (*etwTraceProperties)(unsafe.Pointer(&t.sessionProps[0]))
	procControlTrace.Call(0, uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(props)), eventTraceControlStop)
}

// Stop 停止消费与会话（幂等）。
func (t *EphemeralTracker) Stop() {
	if t.traceHandle != 0 && t.traceHandle != 0xFFFFFFFFFFFFFFFF {
		procCloseTrace.Call(uintptr(t.traceHandle))
		<-t.stopped
		t.traceHandle = 0
	}
	t.stopSession()
}
