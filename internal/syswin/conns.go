//go:build windows

package syswin

// 端口→进程归属：GetExtendedTcpTable / GetExtendedUdpTable。标准权限即可
// 调用，返回全部端点及其归属 PID（与 netstat -ano、资源监视器同源）。
// x/sys 未封装这两个 API，这里手写调用。
//
// 行布局不依赖 Go 结构体对齐，一律按字节解析（encoding/binary）：这些
// Win32 结构是紧凑排列的——表头 dwNumEntries 之后紧跟行数组。
//
// 两处易错点（行布局已对照微软文档逐字段核实）：
//   - 端口字段是网络字节序：取低 16 位后需交换字节；
//   - IPv6 的 TCP 行里 dwState 不在开头而在倒数第二（IPv4 行才是首字段）。

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/lomehong/reshmon/internal/model"
)

var (
	modIphlp           = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtTcpTable = modIphlp.NewProc("GetExtendedTcpTable")
	procGetExtUdpTable = modIphlp.NewProc("GetExtendedUdpTable")
)

const (
	afInet  = 2  // AF_INET
	afInet6 = 23 // AF_INET6
	// TCP_TABLE_OWNER_PID_ALL：监听 + 已建立 + 其余状态，全部带归属 PID
	tcpTableOwnerPIDAll = 5
	// UDP_TABLE_OWNER_PID
	udpTableOwnerPID   = 1
	errInsufficientBuf = 122   // ERROR_INSUFFICIENT_BUFFER
	maxConnEntries     = 20000 // 防御上限，超出截断

	connRowTCP4 = 24 // MIB_TCPROW_OWNER_PID
	connRowTCP6 = 56 // MIB_TCP6ROW_OWNER_PID
	connRowUDP4 = 12 // MIB_UDPROW_OWNER_PID
	connRowUDP6 = 28 // MIB_UDP6ROW_OWNER_PID
)

// tcpStateNames MIB_TCP_STATE 数值 → 名称。取值为标准 MIB-II tcpConnState；
// 由 TestEnumConnsOwnsListeningPort（自建监听端点枚举为 LISTEN）验证。
var tcpStateNames = map[uint32]string{
	1: "CLOSED", 2: "LISTEN", 3: "SYN_SENT", 4: "SYN_RCVD", 5: "ESTAB",
	6: "FIN_WAIT1", 7: "FIN_WAIT2", 8: "CLOSE_WAIT", 9: "CLOSING",
	10: "LAST_ACK", 11: "TIME_WAIT", 12: "DELETE_TCB",
}

const tcpStateListen = 2 // MIB_TCP_STATE_LISTEN

func tcpStateName(state uint32) string {
	if s, ok := tcpStateNames[state]; ok {
		return s
	}
	return fmt.Sprintf("STATE_%d", state) // 未知状态如实显示数值，不猜
}

// queryTable 两段式调用：先取所需大小，再取数据。两次调用之间端点表
// 可能变大（新连接），所以重试几次并预留余量。
func queryTable(proc *windows.LazyProc, af, class uintptr) ([]byte, error) {
	var size uint32
	r, _, _ := proc.Call(0, uintptr(unsafe.Pointer(&size)), 0, af, class, 0)
	if uint32(r) == 0 {
		return nil, nil // 空表
	}
	if uint32(r) != errInsufficientBuf {
		return nil, fmt.Errorf("查询端点表大小失败 0x%X", uint32(r))
	}
	for try := 0; try < 4; try++ {
		buf := make([]byte, size+4096)
		got := uint32(len(buf))
		r, _, _ = proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&got)), 0, af, class, 0)
		if uint32(r) == 0 {
			return buf[:got], nil
		}
		if uint32(r) != errInsufficientBuf {
			return nil, fmt.Errorf("读取端点表失败 0x%X", uint32(r))
		}
		size = got
	}
	return nil, fmt.Errorf("端点表持续变大，重试耗尽")
}

// EnumConns 枚举全部 TCP/UDP 端点（IPv4 + IPv6）及其归属 PID。
func EnumConns() ([]model.ConnStat, error) {
	tables := []struct {
		proc  *windows.LazyProc
		af    uintptr
		class uintptr
		parse func([]byte) []model.ConnStat
	}{
		{procGetExtTcpTable, afInet, tcpTableOwnerPIDAll, parseTCP4Table},
		{procGetExtTcpTable, afInet6, tcpTableOwnerPIDAll, parseTCP6Table},
		{procGetExtUdpTable, afInet, udpTableOwnerPID, parseUDP4Table},
		{procGetExtUdpTable, afInet6, udpTableOwnerPID, parseUDP6Table},
	}
	out := make([]model.ConnStat, 0, 256)
	for _, t := range tables {
		buf, err := queryTable(t.proc, t.af, t.class)
		if err != nil {
			return nil, err
		}
		out = append(out, t.parse(buf)...)
	}
	return out, nil
}

// tableRows 取表头声明的行数，并钳制在缓冲实际容纳的范围内——
// 行数来自系统返回的缓冲，一律校验，不信任其声明。
func tableRows(buf []byte, rowSize int) int {
	if len(buf) < 4 {
		return 0
	}
	n := int(binary.LittleEndian.Uint32(buf[0:4]))
	if avail := (len(buf) - 4) / rowSize; n > avail {
		n = avail
	}
	if n > maxConnEntries {
		n = maxConnEntries
	}
	return n
}

// rows 迭代表内的完整行；尾部残缺的半行直接忽略。
func rows(buf []byte, rowSize int, fn func(row []byte)) {
	n := tableRows(buf, rowSize)
	for i := 4; i+rowSize <= len(buf) && n > 0; i, n = i+rowSize, n-1 {
		fn(buf[i : i+rowSize])
	}
}

// portFromRaw 网络字节序端口字段（DWORD 的低 16 位）→ 主机序端口。
// 缓冲里的字节序是 [端口高 8 位, 端口低 8 位, 0, 0]。
func portFromRaw(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func hostPort(ip net.IP, port uint16) string {
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}

// 行内字段偏移（字节）。TCP 的 IPv4 行与 IPv6 行字段顺序不同：
// IPv4 以 dwState 开头，IPv6 的 dwState 在倒数第二。
const (
	// MIB_TCPROW_OWNER_PID: dwState dwLocalAddr dwLocalPort dwRemoteAddr dwRemotePort dwOwningPid
	tcp4State, tcp4LocalAddr, tcp4LocalPort = 0, 4, 8
	tcp4RemoteAddr, tcp4RemotePort, tcp4PID = 12, 16, 20

	// MIB_TCP6ROW_OWNER_PID: ucLocalAddr[16] dwLocalScopeId dwLocalPort ucRemoteAddr[16]
	//                        dwRemoteScopeId dwRemotePort dwState dwOwningPid
	// 逐字节累加：16 scope → 20 local port → 24 remote addr → 40 scope →
	// 44 remote port → 48 state → 52 pid。两个 scope id 各占 4 字节，漏算
	// 会让其后字段全部错位（实测表现为 IPv6 状态读出 STATE_0）。
	tcp6LocalAddr, tcp6LocalPort   = 0, 20
	tcp6RemoteAddr, tcp6RemotePort = 24, 44
	tcp6State, tcp6PID             = 48, 52

	// MIB_UDPROW_OWNER_PID: dwLocalAddr dwLocalPort dwOwningPid
	udp4LocalAddr, udp4LocalPort, udp4PID = 0, 4, 8

	// MIB_UDP6ROW_OWNER_PID: ucLocalAddr[16] dwLocalScopeId dwLocalPort dwOwningPid
	udp6LocalAddr, udp6LocalPort, udp6PID = 0, 16, 24

	addr4Len, addr6Len = 4, 16
	portLen            = 4 // 端口字段是 DWORD（仅低 16 位有效）
)

func ipv4At(row []byte, off int) net.IP {
	return net.IPv4(row[off], row[off+1], row[off+2], row[off+3])
}

func parseTCP4Table(buf []byte) []model.ConnStat {
	out := make([]model.ConnStat, 0, tableRows(buf, connRowTCP4))
	rows(buf, connRowTCP4, func(row []byte) {
		state := binary.LittleEndian.Uint32(row[tcp4State:])
		c := model.ConnStat{
			Proto: "TCP",
			Local: hostPort(ipv4At(row, tcp4LocalAddr), portFromRaw(row[tcp4LocalPort:tcp4LocalPort+portLen])),
			State: tcpStateName(state),
			PID:   binary.LittleEndian.Uint32(row[tcp4PID:]),
		}
		if state != tcpStateListen { // 非 LISTEN 才有对端
			c.Remote = hostPort(ipv4At(row, tcp4RemoteAddr), portFromRaw(row[tcp4RemotePort:tcp4RemotePort+portLen]))
		}
		out = append(out, c)
	})
	return out
}

func parseTCP6Table(buf []byte) []model.ConnStat {
	out := make([]model.ConnStat, 0, tableRows(buf, connRowTCP6))
	rows(buf, connRowTCP6, func(row []byte) {
		state := binary.LittleEndian.Uint32(row[tcp6State:])
		c := model.ConnStat{
			Proto: "TCP6",
			Local: hostPort(net.IP(row[tcp6LocalAddr:tcp6LocalAddr+addr6Len]), portFromRaw(row[tcp6LocalPort:tcp6LocalPort+portLen])),
			State: tcpStateName(state),
			PID:   binary.LittleEndian.Uint32(row[tcp6PID:]),
		}
		if state != tcpStateListen {
			c.Remote = hostPort(net.IP(row[tcp6RemoteAddr:tcp6RemoteAddr+addr6Len]), portFromRaw(row[tcp6RemotePort:tcp6RemotePort+portLen]))
		}
		out = append(out, c)
	})
	return out
}

func parseUDP4Table(buf []byte) []model.ConnStat {
	out := make([]model.ConnStat, 0, tableRows(buf, connRowUDP4))
	rows(buf, connRowUDP4, func(row []byte) {
		out = append(out, model.ConnStat{
			Proto: "UDP",
			Local: hostPort(ipv4At(row, udp4LocalAddr), portFromRaw(row[udp4LocalPort:udp4LocalPort+portLen])),
			PID:   binary.LittleEndian.Uint32(row[udp4PID:]),
		})
	})
	return out
}

func parseUDP6Table(buf []byte) []model.ConnStat {
	out := make([]model.ConnStat, 0, tableRows(buf, connRowUDP6))
	rows(buf, connRowUDP6, func(row []byte) {
		out = append(out, model.ConnStat{
			Proto: "UDP6",
			Local: hostPort(net.IP(row[udp6LocalAddr:udp6LocalAddr+addr6Len]), portFromRaw(row[udp6LocalPort:udp6LocalPort+portLen])),
			PID:   binary.LittleEndian.Uint32(row[udp6PID:]),
		})
	})
	return out
}
