// Package model 定义全工具共享的数据结构：一次采样的完整快照、
// 每 tick 的总量记录（落盘与图表数据源）、进程/Agent 聚合与健康结论。
package model

import (
	"strconv"
	"time"
)

// 三档健康状态。
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusSat  = "sat"
)

// StatusCN 状态的中文显示名。
func StatusCN(s string) string {
	switch s {
	case StatusOK:
		return "健康"
	case StatusWarn:
		return "亚健康"
	case StatusSat:
		return "饱和"
	}
	return s
}

// Snapshot 单次采样的完整结果。
type Snapshot struct {
	TS        time.Time
	Interval  time.Duration // 本次差分所用的实际间隔（首次采样为 0，速率为零值）
	CPU       CPUStat
	Mem       MemStat
	Disks     []DiskStat
	GPUs      []GPUStat
	NICs      []NICStat
	Procs     []ProcStat // 全量进程（含归因结果）
	Agents    []AgentStat
	CapErrors []string // 采集能力降级说明（如 GPU 计数器缺失）
}

type CPUStat struct {
	BusyPct float64   `json:"busyPct"` // 全核均值 0-100
	Cores   int       `json:"cores"`
	PerCore []float64 `json:"perCore,omitempty"` // 每核 0-100
}

type MemStat struct {
	PhysTotalB   uint64  `json:"physTotalB"`
	PhysAvailB   uint64  `json:"physAvailB"`
	PhysUsedPct  float64 `json:"physUsedPct"`
	CommitTotalB uint64  `json:"commitTotalB"` // 系统提交总量（含物理+页面文件）
	CommitLimitB uint64  `json:"commitLimitB"`
	CommitPct    float64 `json:"commitPct"`
	Handles      uint64  `json:"handles"`
	ProcessCount uint32  `json:"processCount"`
	ThreadCount  uint32  `json:"threadCount"`
}

type DiskStat struct {
	ID         string  `json:"id"` // 物理盘实例，如 "0 C:"
	Letters    string  `json:"l"`  // 盘符列表，如 "C:" / "D: E:"
	ReadBps    float64 `json:"rb"`
	WriteBps   float64 `json:"wb"`
	BusyPct    float64 `json:"busy"` // 100 - %Idle Time，截断到 0-100
	Queue      float64 `json:"q"`    // Current Disk Queue Length
	ReadLatMs  float64 `json:"rms"`
	WriteLatMs float64 `json:"wms"`
	ReadsPs    float64 `json:"rps"`
	WritesPs   float64 `json:"wps"`
}

type GPUStat struct {
	LUIDKey         string  `json:"luid"` // 归一化 LUID，与 GPU 性能计数器实例对应
	Name            string  `json:"name"` // DXGI 名称；不可得时为 "GPU <luid>"
	Vendor          string  `json:"vendor"`
	UtilPct         float64 `json:"utilPct"`    // 引擎利用率合计，可超 100（多引擎并行）
	UtilCapPct      float64 `json:"utilCapPct"` // 显示用，截断 0-100
	DedicatedUsedB  uint64  `json:"dedUsedB"`
	SharedUsedB     uint64  `json:"shUsedB"`
	TotalCommittedB uint64  `json:"committedB"`
	DedicatedTotalB uint64  `json:"dedTotalB"` // DXGI DedicatedVideoMemory；核显可能为 0
	TempC           int     `json:"tempC"`     // NVML 温度（仅 NVIDIA）；不可得为 -1
}

type NICStat struct {
	Name    string  `json:"name"`
	RecvBps float64 `json:"recvBps"`
	SentBps float64 `json:"sentBps"`
}

type ProcStat struct {
	PID      uint32  `json:"pid"`
	PPID     uint32  `json:"ppid"`
	Name     string  `json:"n"`
	Path     string  `json:"path,omitempty"`
	Agent    string  `json:"a,omitempty"`
	CpuPct   float64 `json:"cpu"`  // 占全机 CPU 容量的百分比
	MemWSB   uint64  `json:"ws"`   // 工作集
	MemPrivB uint64  `json:"priv"` // 私有提交内存
	ReadBps  float64 `json:"rb"`
	WriteBps float64 `json:"wb"`
	GpuPct   float64 `json:"gpu,omitempty"`
	VramB    uint64  `json:"vram,omitempty"`
}

type AgentStat struct {
	Name     string  `json:"n"`
	CpuPct   float64 `json:"cpu"`
	MemPrivB uint64  `json:"priv"`
	ReadBps  float64 `json:"rb"`
	WriteBps float64 `json:"wb"`
	GpuPct   float64 `json:"gpu"`
	VramB    uint64  `json:"vram"`
	Procs    int     `json:"p"`
}

// Totals 每 tick 的总量记录：环形缓冲、JSONL 落盘与图表共用的数据源。
type Totals struct {
	T          int64             `json:"t"` // unix 毫秒
	CPU        float64           `json:"cpu"`
	MemUsedPct float64           `json:"memp"`
	CommitPct  float64           `json:"commit"`
	Disks      []DiskTotal       `json:"disks,omitempty"`
	GPUs       []GPUTotal        `json:"gpus,omitempty"`
	NetRecvBps float64           `json:"nr,omitempty"`
	NetSentBps float64           `json:"ns,omitempty"`
	Agents     []AgentStat       `json:"agents,omitempty"`
	Health     map[string]string `json:"h,omitempty"`
}

type DiskTotal struct {
	ID    string  `json:"id"`
	L     string  `json:"l"`
	Busy  float64 `json:"busy"`
	RBps  float64 `json:"rb"`
	WBps  float64 `json:"wb"`
	LatMs float64 `json:"lat"` // 读写延迟均值
}

type GPUTotal struct {
	Key     string  `json:"k"` // 显示名
	Util    float64 `json:"u"`
	VramPct float64 `json:"vp"`
}

// FmtBytes 人类可读的字节数。
func FmtBytes(b float64) string {
	switch {
	case b >= 1<<40:
		return strconv.FormatFloat(b/(1<<40), 'f', 1, 64) + " TB"
	case b >= 1<<30:
		return strconv.FormatFloat(b/(1<<30), 'f', 1, 64) + " GB"
	case b >= 1<<20:
		return strconv.FormatFloat(b/(1<<20), 'f', 1, 64) + " MB"
	case b >= 1<<10:
		return strconv.FormatFloat(b/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.FormatFloat(b, 'f', 0, 64) + " B"
	}
}
