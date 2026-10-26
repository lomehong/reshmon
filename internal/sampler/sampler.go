//go:build windows

// Package sampler 编排一次完整采样：PDH 性能计数器（CPU/磁盘/GPU/网络）
// + Win32 API（进程/内存）。PDH 走 RAW 路线，速率由相邻两帧差分自算，
// 口径统一且天然支持动态实例（GPU 引擎按进程增减）。
package sampler

import (
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lomehong/reshmon/internal/attrib"
	"github.com/lomehong/reshmon/internal/model"
	"github.com/lomehong/reshmon/internal/syswin"
)

const (
	pathCPU       = `\Processor(*)\% Processor Time`
	pathGPUEngine = `\GPU Engine(*)\Utilization Percentage`
)

var diskPaths = map[string]string{
	"rb":   `\PhysicalDisk(*)\Disk Read Bytes/sec`,
	"wb":   `\PhysicalDisk(*)\Disk Write Bytes/sec`,
	"idle": `\PhysicalDisk(*)\% Idle Time`,
	"q":    `\PhysicalDisk(*)\Current Disk Queue Length`,
	"rms":  `\PhysicalDisk(*)\Avg. Disk sec/Read`,
	"wms":  `\PhysicalDisk(*)\Avg. Disk sec/Write`,
	"rps":  `\PhysicalDisk(*)\Disk Reads/sec`,
	"wps":  `\PhysicalDisk(*)\Disk Writes/sec`,
}

var gpuProcMemPaths = map[string]string{
	"local": `\GPU Process Memory(*)\Local Usage`,
}

var gpuAdaptMemPaths = map[string]string{
	"ded":    `\GPU Adapter Memory(*)\Dedicated Usage`,
	"shared": `\GPU Adapter Memory(*)\Shared Usage`,
	"commit": `\GPU Adapter Memory(*)\Total Committed`,
}

var netPaths = map[string]string{
	"recv": `\Network Interface(*)\Bytes Received/sec`,
	"sent": `\Network Interface(*)\Bytes Sent/sec`,
}

// rawVal 一帧 RAW 值；ts 为采集时间戳（100ns）。
type rawVal struct {
	first, second, ts int64
}

type rawSet map[string]rawVal

// Sampler 有状态采样器；Tick 必须串行调用。
type Sampler struct {
	interval time.Duration

	q *syswin.PDHQuery

	cCPU    *syswin.PDHCounter
	cDisk   map[string]*syswin.PDHCounter
	cGPUEng *syswin.PDHCounter
	cGPUMem map[string]*syswin.PDHCounter
	cGPUAd  map[string]*syswin.PDHCounter
	cNet    map[string]*syswin.PDHCounter

	capErrs []string
	capSeen map[string]bool

	prevRaw map[string]rawSet

	adapterCache []syswin.DXGIAdapter
	adapterAt    time.Time
	nvmlCache    []syswin.NVMLDevice
	nvmlAt       time.Time

	numCPU int

	prevAt    time.Time
	prevProcs map[uint32]procAcc

	engine *attrib.Engine
}

type procAcc struct {
	cpu    uint64
	readB  uint64
	writeB uint64
}

// New 创建采样器并预热一帧（速率自第二帧起可算）。
// 个别计数器路径在本机不存在（如无 GPU）只降级不报错。
func New(interval time.Duration, patterns []attrib.Pattern) (*Sampler, error) {
	q, err := syswin.PDHOpen()
	if err != nil {
		return nil, err
	}
	s := &Sampler{
		interval:  interval,
		q:         q,
		cDisk:     map[string]*syswin.PDHCounter{},
		cGPUMem:   map[string]*syswin.PDHCounter{},
		cGPUAd:    map[string]*syswin.PDHCounter{},
		cNet:      map[string]*syswin.PDHCounter{},
		capSeen:   map[string]bool{},
		prevRaw:   map[string]rawSet{},
		numCPU:    runtime.NumCPU(),
		prevProcs: map[uint32]procAcc{},
		engine:    attrib.New(patterns),
	}
	if s.numCPU < 1 {
		s.numCPU = 1
	}
	add := func(dst **syswin.PDHCounter, path string) {
		c, err := q.Add(path)
		if err != nil {
			s.noteCap(err.Error())
			return
		}
		*dst = c
	}
	addMap := func(dst map[string]*syswin.PDHCounter, paths map[string]string) {
		for k, p := range paths {
			c, err := q.Add(p)
			if err != nil {
				s.noteCap(err.Error())
				continue
			}
			dst[k] = c
		}
	}
	add(&s.cCPU, pathCPU)
	addMap(s.cDisk, diskPaths)
	add(&s.cGPUEng, pathGPUEngine)
	addMap(s.cGPUMem, gpuProcMemPaths)
	addMap(s.cGPUAd, gpuAdaptMemPaths)
	addMap(s.cNet, netPaths)

	if err := q.Collect(); err != nil && !syswin.IsPDHSoft(err) {
		s.noteCap(err.Error())
	}
	return s, nil
}

func (s *Sampler) Close() { s.q.Close() }

func (s *Sampler) noteCap(msg string) {
	if s.capSeen[msg] {
		return
	}
	s.capSeen[msg] = true
	s.capErrs = append(s.capErrs, msg)
}

// Interval 采样间隔（UI 提示用；速率差分用真实时间戳）。
func (s *Sampler) Interval() time.Duration { return s.interval }

// Tick 执行一次采集并返回完整快照。
func (s *Sampler) Tick() *model.Snapshot {
	now := time.Now()
	collectErr := s.q.Collect()
	snap := &model.Snapshot{TS: now, CPU: model.CPUStat{Cores: s.numCPU}}

	cur := map[string]rawSet{}
	s.readCPU(snap, cur)
	s.readDisks(snap, cur)
	gpuByPid, vramByPid := s.readGPUs(snap, cur)
	s.readNet(snap, cur)
	s.prevRaw = cur

	s.readMem(snap)
	s.readProcs(snap, now, gpuByPid, vramByPid)

	if collectErr != nil && !syswin.IsPDHSoft(collectErr) {
		s.noteCap(collectErr.Error())
	}
	if len(s.capErrs) > 0 {
		snap.CapErrors = append([]string(nil), s.capErrs...)
	}
	return snap
}

// raw 读取计数器当前帧，登记进 cur，返回当前帧。
func (s *Sampler) raw(key string, c *syswin.PDHCounter, cur map[string]rawSet) rawSet {
	if c == nil {
		return nil
	}
	items, err := c.RawItems()
	if err != nil {
		s.noteCap(err.Error())
		return nil
	}
	set := rawSet{}
	for _, it := range items {
		set[it.Name] = rawVal{first: it.First, second: it.Second, ts: it.TS}
	}
	cur[key] = set
	return set
}

// timerPct: PERF_100NSEC_TIMER 差分 → 百分比（first 为累计活跃时间）。
func timerPct(a, b rawVal) float64 {
	if b.ts <= a.ts {
		return 0
	}
	return float64(b.first-a.first) / float64(b.ts-a.ts) * 100
}

// bulkRate: PERF_COUNTER_BULK_COUNT 差分 → 每秒速率。
func bulkRate(a, b rawVal) float64 {
	if b.ts <= a.ts {
		return 0
	}
	return float64(b.first-a.first) / (float64(b.ts-a.ts) / 1e7)
}

// avgTimerMs: PERF_AVERAGE_TIMER 差分 → 每次操作平均毫秒
// （first 为累计时间 100ns，second 为累计次数）。
func avgTimerMs(a, b rawVal) float64 {
	if b.second <= a.second {
		return 0
	}
	return float64(b.first-a.first) / float64(b.second-a.second) / 1e4
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func (s *Sampler) readCPU(snap *model.Snapshot, cur map[string]rawSet) {
	set := s.raw("cpu", s.cCPU, cur)
	if set == nil {
		return
	}
	prev := s.prevRaw["cpu"]
	for name, now := range set {
		p, ok := prev[name]
		if !ok {
			continue
		}
		v := clamp(timerPct(p, now), 0, 100)
		if name == "_Total" {
			snap.CPU.BusyPct = v
			continue
		}
		if n, err := strconv.Atoi(name); err == nil && n >= 0 && n < 1024 {
			for len(snap.CPU.PerCore) <= n {
				snap.CPU.PerCore = append(snap.CPU.PerCore, 0)
			}
			snap.CPU.PerCore[n] = v
		}
	}
}

func (s *Sampler) readDisks(snap *model.Snapshot, cur map[string]rawSet) {
	type diskAgg struct {
		busy, rb, wb, q, rms, wms, rps, wps float64
	}
	ags := map[string]*diskAgg{}
	pair := func(key string, f func(a *diskAgg, v float64)) {
		set := s.raw("disk:"+key, s.cDisk[key], cur)
		if set == nil {
			return
		}
		prev := s.prevRaw["disk:"+key]
		for name, now := range set {
			if name == "_Total" {
				continue
			}
			p, ok := prev[name]
			if !ok {
				continue
			}
			a := ags[name]
			if a == nil {
				a = &diskAgg{}
				ags[name] = a
			}
			f(a, timerOrRaw(key, p, now))
		}
	}
	// %Idle Time 是反相计时器：差分得到空闲%，忙 = 100 - 空闲。
	pair("idle", func(a *diskAgg, v float64) { a.busy = clamp(100-v, 0, 100) })
	pair("rb", func(a *diskAgg, v float64) { a.rb = max0(v) })
	pair("wb", func(a *diskAgg, v float64) { a.wb = max0(v) })
	pair("q", func(a *diskAgg, v float64) { a.q = max0(v) })
	pair("rms", func(a *diskAgg, v float64) { a.rms = max0(v) })
	pair("wms", func(a *diskAgg, v float64) { a.wms = max0(v) })
	pair("rps", func(a *diskAgg, v float64) { a.rps = max0(v) })
	pair("wps", func(a *diskAgg, v float64) { a.wps = max0(v) })

	for id, a := range ags {
		snap.Disks = append(snap.Disks, model.DiskStat{
			ID:         id,
			Letters:    lettersOf(id),
			ReadBps:    a.rb,
			WriteBps:   a.wb,
			BusyPct:    a.busy,
			Queue:      a.q,
			ReadLatMs:  a.rms,
			WriteLatMs: a.wms,
			ReadsPs:    a.rps,
			WritesPs:   a.wps,
		})
	}
	sort.Slice(snap.Disks, func(i, j int) bool { return snap.Disks[i].ID < snap.Disks[j].ID })
}

// timerOrRaw 按计数器键选差分口径：q 为瞬时值，rms/wms 为均次毫秒，
// idle 为反相计时器百分比，其余为每秒速率。
func timerOrRaw(key string, p, now rawVal) float64 {
	switch key {
	case "q":
		return float64(now.first)
	case "rms", "wms":
		return avgTimerMs(p, now)
	case "idle":
		return timerPct(p, now)
	default:
		return bulkRate(p, now)
	}
}

func lettersOf(id string) string {
	f := strings.Fields(id)
	if len(f) <= 1 {
		return ""
	}
	return strings.Join(f[1:], " ")
}

func (s *Sampler) readGPUs(snap *model.Snapshot, cur map[string]rawSet) (gpuByPid map[uint32]float64, vramByPid map[uint32]uint64) {
	gpuByPid = map[uint32]float64{}
	vramByPid = map[uint32]uint64{}
	engLuid := map[string]float64{}

	set := s.raw("gpueng", s.cGPUEng, cur)
	if set != nil {
		prev := s.prevRaw["gpueng"]
		for name, now := range set {
			p, ok := prev[name]
			if !ok {
				continue
			}
			pid, luid, ok2 := ParseGPUEngineInstance(name)
			if !ok2 {
				continue
			}
			v := timerPct(p, now) // 单引擎 0-100，多引擎并行可累加超 100
			if v < 0 || v > 2000 {
				continue
			}
			gpuByPid[pid] += v
			engLuid[luid] += v
		}
	}
	if c := s.cGPUMem["local"]; c != nil {
		mset := s.raw("gpumem:local", c, cur)
		for name, now := range mset {
			if now.first < 0 {
				continue
			}
			pid, _, ok := ParseGPUEngineInstance(name)
			if !ok {
				continue
			}
			vramByPid[pid] += uint64(now.first)
		}
	}

	adapt := map[string]map[string]uint64{}
	for k, c := range s.cGPUAd {
		aset := s.raw("gpuad:"+k, c, cur)
		for name, now := range aset {
			luid, ok := ParseLUIDInstance(name)
			if !ok || now.first < 0 {
				continue
			}
			m := adapt[luid]
			if m == nil {
				m = map[string]uint64{}
				adapt[luid] = m
			}
			m[k] = uint64(now.first)
		}
	}

	luids := map[string]bool{}
	for l := range engLuid {
		luids[l] = true
	}
	adapters := s.adapters()
	nvmlDevs := s.nvml()
	named := map[string]bool{}
	for _, a := range adapters {
		luids[a.LUIDKey] = true
		named[a.LUIDKey] = true
	}
	// 未被 DXGI 命名且无引擎活动的 LUID（WARP/间接适配器的内存实例）视为噪音。
	for l := range luids {
		if !named[l] && engLuid[l] <= 0 {
			delete(luids, l)
		}
	}
	nvidiaSeen := 0
	for l := range luids {
		name := "GPU " + l
		vendor, vramTotal := "", uint64(0)
		for _, a := range adapters {
			if a.LUIDKey == l {
				name, vendor, vramTotal = a.Description, a.Vendor, a.DedicatedVRAMB
				break
			}
		}
		temp := -1
		if vendor == "NVIDIA" && nvidiaSeen < len(nvmlDevs) {
			temp = nvmlDevs[nvidiaSeen].TempC
			nvidiaSeen++
		}
		m := adapt[l]
		var ded, shared, commit uint64
		if m != nil {
			ded, shared, commit = m["ded"], m["shared"], m["commit"]
		}
		util := engLuid[l]
		snap.GPUs = append(snap.GPUs, model.GPUStat{
			LUIDKey:         l,
			Name:            name,
			Vendor:          vendor,
			UtilPct:         max0(util),
			UtilCapPct:      clamp(util, 0, 100),
			DedicatedUsedB:  ded,
			SharedUsedB:     shared,
			TotalCommittedB: commit,
			DedicatedTotalB: vramTotal,
			TempC:           temp,
		})
	}
	sort.Slice(snap.GPUs, func(i, j int) bool { return snap.GPUs[i].LUIDKey < snap.GPUs[j].LUIDKey })
	return gpuByPid, vramByPid
}

// nameOf 已删除：gpu mem 集合遍历时直接以 map 键为实例名。

func (s *Sampler) adapters() []syswin.DXGIAdapter {
	if !s.adapterAt.IsZero() && time.Since(s.adapterAt) < 5*time.Minute {
		return s.adapterCache
	}
	a, err := syswin.EnumDXGIAdapters()
	if err != nil {
		s.noteCap("GPU 适配器命名不可用：" + err.Error())
	} else {
		s.adapterCache = a
	}
	s.adapterAt = time.Now()
	return s.adapterCache
}

func (s *Sampler) nvml() []syswin.NVMLDevice {
	if !s.nvmlAt.IsZero() && time.Since(s.nvmlAt) < 30*time.Second {
		return s.nvmlCache
	}
	s.nvmlCache = syswin.NVMLDevices()
	s.nvmlAt = time.Now()
	return s.nvmlCache
}

func (s *Sampler) readNet(snap *model.Snapshot, cur map[string]rawSet) {
	type nacc struct {
		recv, sent float64
	}
	ags := map[string]*nacc{}
	for k, c := range s.cNet {
		set := s.raw("net:"+k, c, cur)
		if set == nil {
			continue
		}
		prev := s.prevRaw["net:"+k]
		for name, now := range set {
			p, ok := prev[name]
			if !ok {
				continue
			}
			a := ags[name]
			if a == nil {
				a = &nacc{}
				ags[name] = a
			}
			if k == "recv" {
				a.recv = max0(bulkRate(p, now))
			} else {
				a.sent = max0(bulkRate(p, now))
			}
		}
	}
	for name, a := range ags {
		snap.NICs = append(snap.NICs, model.NICStat{Name: name, RecvBps: a.recv, SentBps: a.sent})
	}
	sort.Slice(snap.NICs, func(i, j int) bool {
		return snap.NICs[i].RecvBps+snap.NICs[i].SentBps > snap.NICs[j].RecvBps+snap.NICs[j].SentBps
	})
}

func (s *Sampler) readMem(snap *model.Snapshot) {
	m, err := syswin.ReadMemStatOS()
	if err != nil {
		s.noteCap("系统内存读取失败：" + err.Error())
		return
	}
	snap.Mem = model.MemStat{
		PhysTotalB:   m.PhysTotalB,
		PhysAvailB:   m.PhysAvailB,
		PhysUsedPct:  m.PhysLoadPct,
		CommitTotalB: m.CommitTotalB,
		CommitLimitB: m.CommitLimitB,
		Handles:      m.HandleCount,
		ProcessCount: uint32(m.ProcessCount),
		ThreadCount:  uint32(m.ThreadCount),
	}
	if m.CommitLimitB > 0 {
		snap.Mem.CommitPct = float64(m.CommitTotalB) / float64(m.CommitLimitB) * 100
	}
}

func (s *Sampler) readProcs(snap *model.Snapshot, now time.Time, gpuByPid map[uint32]float64, vramByPid map[uint32]uint64) {
	dt := 0.0
	if !s.prevAt.IsZero() {
		dt = now.Sub(s.prevAt).Seconds()
	}
	infos, err := syswin.EnumProcs()
	if err != nil {
		s.noteCap("进程枚举失败：" + err.Error())
		return
	}
	procs := make([]model.ProcStat, 0, len(infos))
	next := make(map[uint32]procAcc, len(infos))
	for _, in := range infos {
		p := model.ProcStat{
			PID:      in.PID,
			PPID:     in.PPID,
			Name:     in.Name,
			Path:     in.Path,
			MemWSB:   in.WorkingSetB,
			MemPrivB: in.PrivateB,
		}
		if g := gpuByPid[in.PID]; g > 0 {
			p.GpuPct = clamp(g, 0, 100)
		}
		p.VramB = vramByPid[in.PID]
		if prev, ok := s.prevProcs[in.PID]; ok && dt > 0.05 {
			if in.CPU100ns >= prev.cpu {
				p.CpuPct = clamp(float64(in.CPU100ns-prev.cpu)/(dt*1e7)*100/float64(s.numCPU), 0, 100)
			}
			if in.ReadB >= prev.readB {
				p.ReadBps = float64(in.ReadB-prev.readB) / dt
			}
			if in.WriteB >= prev.writeB {
				p.WriteBps = float64(in.WriteB-prev.writeB) / dt
			}
		}
		next[in.PID] = procAcc{cpu: in.CPU100ns, readB: in.ReadB, writeB: in.WriteB}
		procs = append(procs, p)
	}
	s.prevProcs = next
	s.prevAt = now
	snap.Interval = time.Duration(dt * float64(time.Second))

	s.engine.Attribute(procs)
	snap.Procs = procs
	snap.Agents = attrib.Aggregate(procs)
}

var (
	reGPUPidLUID = regexp.MustCompile(`pid_(\d+)_luid_0x([0-9A-Fa-f]+)_0x([0-9A-Fa-f]+)`)
	reGPULUID    = regexp.MustCompile(`luid_0x([0-9A-Fa-f]+)_0x([0-9A-Fa-f]+)`)
)

// ParseGPUEngineInstance 解析 GPU Engine / GPU Process Memory 实例名，
// 形如 pid_1234_luid_0x00000000_0x0000C437_phys_0_eng_0_engtype_3D。
func ParseGPUEngineInstance(s string) (pid uint32, luid string, ok bool) {
	m := reGPUPidLUID.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	v, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return 0, "", false
	}
	high, err1 := strconv.ParseUint(m[2], 16, 32)
	low, err2 := strconv.ParseUint(m[3], 16, 32)
	if err1 != nil || err2 != nil {
		return 0, "", false
	}
	return uint32(v), syswin.LUIDKey(uint32(high), uint32(low)), true
}

// ParseLUIDInstance 解析 GPU Adapter Memory 实例名（luid_0x.._0x.._phys_0）。
func ParseLUIDInstance(s string) (string, bool) {
	m := reGPULUID.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	high, err1 := strconv.ParseUint(m[1], 16, 32)
	low, err2 := strconv.ParseUint(m[2], 16, 32)
	if err1 != nil || err2 != nil {
		return "", false
	}
	return syswin.LUIDKey(uint32(high), uint32(low)), true
}
