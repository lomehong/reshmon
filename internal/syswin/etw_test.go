//go:build windows && amd64

package syswin

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

func ptrSizeOf[T any](v T) uintptr { return unsafe.Sizeof(v) }

func mkDiskInit(irp uint64) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:8], irp)
	binary.LittleEndian.PutUint32(b[8:12], 4242) // IssuingThreadId（不用）
	return b
}

func mkDiskDone(transfer uint32, irp uint64) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[8:12], transfer) // TransferSize
	binary.LittleEndian.PutUint64(b[32:40], irp)     // Irp
	return b
}

func mkProcStart(ts uint64, pid, ppid uint32, subAuths int, name string) []byte {
	// EPROCESS ptr(8) pid(4) ppid(4) session(4) exit(4) = 24；SID(8+4n)；名字
	b := make([]byte, 24+8+4*subAuths+len(name)+1)
	binary.LittleEndian.PutUint32(b[8:12], pid)
	binary.LittleEndian.PutUint32(b[12:16], ppid)
	sid := 24
	b[sid] = 1 // Revision
	b[sid+1] = byte(subAuths)
	off := (8 + 4*subAuths + 3) &^ 3
	copy(b[sid+off:], name)
	_ = ts
	return b
}

// IRP 配对：Init 记发起进程，完成事件按 Irp 归账（不信任完成事件 header PID）。
func TestIrpPairing(t *testing.T) {
	st := newEtwState()
	st.onDiskInit(100, mkDiskInit(0xA000))
	st.onDiskInit(200, mkDiskInit(0xA001))
	// 完成：IRP 0xA000 属于 100，但事件在"别的"上下文（headerPid=4）
	st.onDiskDone(4, diskEvtRead, mkDiskDone(4096, 0xA000))
	st.onDiskDone(4, diskEvtWrite, mkDiskDone(8192, 0xA001))
	r1, w1, _ := st.Consume(100)
	if r1 != 4096 || w1 != 0 {
		t.Fatalf("pid100 归账错误: r=%d w=%d", r1, w1)
	}
	r2, w2, _ := st.Consume(200)
	if r2 != 0 || w2 != 8192 {
		t.Fatalf("pid200 归账错误: r=%d w=%d", r2, w2)
	}
	// 未配对的 Init（会话中途启动等）：回退用完成事件 header PID
	st.onDiskDone(777, diskEvtRead, mkDiskDone(512, 0xDEAD))
	if r, _, _ := st.Consume(777); r != 512 {
		t.Fatalf("未配对 IO 应回退 header PID: %d", r)
	}
}

// Consume 清零 + TakeDead 只结算退出且不在枚举中的进程。
func TestConsumeAndTakeDead(t *testing.T) {
	st := newEtwState()
	st.onDiskInit(10, mkDiskInit(1))
	st.onDiskDone(10, diskEvtRead, mkDiskDone(1000, 1))
	if r, _, _ := st.Consume(10); r != 1000 {
		t.Fatal("首次 Consume 应返回累计值")
	}
	if r, _, _ := st.Consume(10); r != 0 {
		t.Fatal("重复 Consume 应已清零")
	}
	// 死进程：End 事件 + 不在存活枚举 → 剩余字节一次性结算
	st.onDiskInit(10, mkDiskInit(2))
	st.onDiskDone(10, diskEvtWrite, mkDiskDone(3000, 2))
	st.onProcessEnd(10)
	dead := st.TakeDead(map[uint32]bool{10: false, 999: true})
	if len(dead) != 1 || dead[0].WriteB != 3000 || dead[0].PID != 10 {
		t.Fatalf("死者结算错误: %+v", dead)
	}
	// 存活进程（在枚举中）不被结算
	st.onDiskInit(999, mkDiskInit(3))
	st.onDiskDone(999, diskEvtRead, mkDiskDone(50, 3))
	if d := st.TakeDead(map[uint32]bool{999: true}); len(d) != 0 {
		t.Fatalf("存活进程不应结算: %+v", d)
	}
}

// Process Start 事件解析：PID/PPID/镜像名；PID 复用时旧身份未结算字节先落账。
func TestProcessStartAndReuse(t *testing.T) {
	st := newEtwState()
	st.onProcessStart(1000, mkProcStart(1000, 42, 7, 1, "compiler.exe"))
	st.onDiskInit(42, mkDiskInit(1))
	st.onDiskDone(42, diskEvtRead, mkDiskDone(2048, 1))
	// PID 42 被新进程复用（更晚的启动时间戳）→ 旧身份 2048 字节先结算
	st.onProcessStart(5000, mkProcStart(5000, 42, 9, 5, "svchost.exe"))
	dead := st.TakeDead(map[uint32]bool{})
	if len(dead) != 1 || dead[0].ReadB != 2048 || dead[0].Name != "compiler.exe" {
		t.Fatalf("PID 复用结算错误: %+v", dead)
	}
	st.onDiskInit(42, mkDiskInit(2))
	st.onDiskDone(42, diskEvtWrite, mkDiskDone(64, 2))
	if _, w, _ := st.Consume(42); w != 64 {
		t.Fatal("复用后的新进程应重新累计")
	}
}

// 镜像名解析：SID 子授权数不同 → 名字偏移随之平移；垃圾数据不 panic。
func TestProcessImageName(t *testing.T) {
	if got := processImageName(mkProcStart(0, 1, 2, 2, "node.exe")[24:]); got != "node.exe" {
		t.Fatalf("名字解析失败: %q", got)
	}
	if got := processImageName(mkProcStart(0, 1, 2, 0, "go.exe")[24:]); got != "go.exe" {
		t.Fatalf("无子授权 SID 解析失败: %q", got)
	}
	if got := processImageName([]byte{1, 9, 0, 0}); got != "" {
		t.Fatalf("越界 SID 应返回空: %q", got)
	}
	if got := processImageName([]byte{1, 0, 0, 0, 0x01, 0x02}); got != "" {
		t.Fatalf("不可打印名应返回空: %q", got)
	}
}

// SampledProfile 计数：按 PID 累计；PID 0（空闲）跳过；Consume 清零；
// TakeDead 携带采样数；速率护栏触发后停用。
func TestProfileSamples(t *testing.T) {
	st := newEtwState()
	base := int64(1_000_000) // ms
	for i := 0; i < 30; i++ {
		st.onProfile(100, base+int64(i)) // 每毫秒一样本
		st.onProfile(200, base+int64(i))
		st.onProfile(0, base+int64(i)) // System Idle：必须跳过
	}
	if _, _, s := st.Consume(100); s != 30 {
		t.Fatalf("pid100 采样计数错误: %d", s)
	}
	if _, _, s := st.Consume(100); s != 0 {
		t.Fatalf("Consume 后应清零: %d", s)
	}
	if _, _, s := st.Consume(0); s != 0 {
		t.Fatalf("PID 0 不得累计: %d", s)
	}
	if _, _, s := st.Consume(200); s != 30 { // 清掉 pid200，别混入死者结算
		t.Fatalf("pid200 采样计数错误: %d", s)
	}
	// 死者结算带采样数
	st.onProfile(300, base)
	st.onProfile(300, base+1)
	st.onProcessEnd(300)
	dead := st.TakeDead(map[uint32]bool{})
	if len(dead) != 1 || dead[0].Samples != 2 {
		t.Fatalf("死者采样结算错误: %+v", dead)
	}
	// 速率护栏：同一毫秒内灌 4096+ 事件 → 窗口时长按 ≥1ms 计，速率爆表，
	// 护栏触发；触发前的事件已入账（4095），触发后停账。
	st2 := newEtwState()
	for i := 0; i < 5000; i++ {
		st2.onProfile(777, base) // 全部同一毫秒
	}
	if !st2.ProfileOff() {
		t.Fatal("同毫秒灌爆应触发速率护栏")
	}
	_, _, s2 := st2.Consume(777)
	if s2 < 4000 || s2 > 4095 {
		t.Fatalf("护栏触发前的事件应已入账: %d", s2)
	}
	st2.onProfile(777, base)
	if _, _, s3 := st2.Consume(777); s3 != 0 {
		t.Fatalf("护栏触发后不得继续累计: %d", s3)
	}
}

// 布局钉子：FFI 结构体尺寸漂移在编译期断言之外再由测试双保险。
func TestFFISizes(t *testing.T) {
	if got := ptrSizeOf(etwWnodeHeader{}); got != 48 {
		t.Fatalf("WNODE_HEADER %d", got)
	}
	if got := ptrSizeOf(etwEventRecord{}); got != 112 {
		t.Fatalf("EVENT_RECORD %d", got)
	}
	if got := ptrSizeOf(etwLogfileW{}); got != 448 {
		t.Fatalf("EVENT_TRACE_LOGFILEW %d", got)
	}
}
