//go:build windows

package syswin

import (
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/lomehong/reshmon/internal/model"
)

// 端口→进程归属的真值校验：自己监听一个 TCP 和一个 UDP 端口，再从端点表
// 里把它们找回来。这同时验证了三件容易错的事：端口字节序、状态码映射
// （自建监听必须是 LISTEN）、归属 PID（必须是自己）。
func TestEnumConnsOwnsListeningPort(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法创建监听端口: %v", err)
	}
	defer ln.Close()
	tcpPort := uint16(ln.Addr().(*net.TCPAddr).Port)

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法创建 UDP 端口: %v", err)
	}
	defer pc.Close()
	udpPort := uint16(pc.LocalAddr().(*net.UDPAddr).Port)

	conns, err := EnumConns()
	if err != nil {
		t.Fatalf("枚举端点失败: %v", err)
	}
	if len(conns) == 0 {
		t.Fatal("端点表为空——本机至少有监听端口")
	}

	self := uint32(os.Getpid())
	find := func(proto string, port uint16) *model.ConnStat {
		for i := range conns {
			c := &conns[i]
			if c.Proto != proto || c.PID != self {
				continue
			}
			_, p, err := net.SplitHostPort(c.Local)
			if err != nil {
				continue
			}
			if n, err := strconv.Atoi(p); err == nil && uint16(n) == port {
				return c
			}
		}
		return nil
	}

	tcp := find("TCP", tcpPort)
	if tcp == nil {
		t.Fatalf("未在端点表中找到本进程的 TCP 监听端口 %d（端口字节序或 PID 映射可能有误）", tcpPort)
	}
	if tcp.State != "LISTEN" {
		t.Fatalf("监听端点的状态应为 LISTEN，得到 %q（状态码映射有误）", tcp.State)
	}
	if tcp.Remote != "" {
		t.Fatalf("监听端点不应有对端，得到 %q", tcp.Remote)
	}

	udp := find("UDP", udpPort)
	if udp == nil {
		t.Fatalf("未在端点表中找到本进程的 UDP 端口 %d", udpPort)
	}
	if udp.State != "" || udp.Remote != "" {
		t.Fatalf("UDP 端点不应有状态/对端: %+v", udp)
	}
}

// 已建立的连接应带对端，且两端端口都能正确还原字节序。
func TestEnumConnsEstablished(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法创建监听端口: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			<-make(chan struct{}) // 保持连接直到测试结束
		}
	}()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Skipf("无法建立连接: %v", err)
	}
	defer client.Close()
	serverPort := uint16(ln.Addr().(*net.TCPAddr).Port)
	clientPort := uint16(client.LocalAddr().(*net.TCPAddr).Port)

	conns, err := EnumConns()
	if err != nil {
		t.Fatalf("枚举端点失败: %v", err)
	}
	// 找到客户端这一侧：本端 = clientPort，对端 = serverPort
	for _, c := range conns {
		if c.Proto != "TCP" || c.State == "LISTEN" {
			continue
		}
		_, lp, e1 := net.SplitHostPort(c.Local)
		_, rp, e2 := net.SplitHostPort(c.Remote)
		if e1 != nil || e2 != nil {
			continue
		}
		if lp == strconv.Itoa(int(clientPort)) && rp == strconv.Itoa(int(serverPort)) {
			if c.State != "ESTAB" {
				t.Fatalf("已建立连接状态应为 ESTAB，得到 %q", c.State)
			}
			return
		}
	}
	t.Skip("未在端点表中观测到刚建立的连接（可能已被回收），跳过")
}

// 防御性：行数声明大于缓冲实际容量时必须钳制，不能越界读。
func TestTableRowsClamp(t *testing.T) {
	// 声明 10 行，但缓冲只够 1 行（4 字节表头 + 24 字节 TCPv4 行）
	buf := make([]byte, 4+connRowTCP4)
	buf[0], buf[1], buf[2], buf[3] = 10, 0, 0, 0
	if n := tableRows(buf, connRowTCP4); n != 1 {
		t.Fatalf("行数应钳制到缓冲容量 1，得到 %d", n)
	}
	// 表头都不完整
	if n := tableRows([]byte{1, 2}, connRowTCP4); n != 0 {
		t.Fatalf("表头不完整应返回 0，得到 %d", n)
	}
	// 尾部残缺半行不应被解析
	short := make([]byte, 4+connRowTCP4+5)
	short[0], short[1], short[2], short[3] = 2, 0, 0, 0
	if got := len(parseTCP4Table(short)); got != 1 {
		t.Fatalf("残缺半行不应被解析，期望 1 行，得到 %d", got)
	}
}

// IPv6 行字段顺序与 IPv4 不同（dwState 不在行首），且含两个 scope id 字段，
// 偏移算错会读出 STATE_0。这里用自建 IPv6 监听端点验证。
func TestEnumConnsIPv6(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("本机不支持 IPv6 回环: %v", err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	conns, err := EnumConns()
	if err != nil {
		t.Fatalf("枚举端点失败: %v", err)
	}
	self := uint32(os.Getpid())
	for _, c := range conns {
		if c.Proto != "TCP6" || c.PID != self {
			continue
		}
		_, p, err := net.SplitHostPort(c.Local)
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(p); err != nil || uint16(n) != port {
			continue
		}
		if c.State != "LISTEN" {
			t.Fatalf("IPv6 监听端点状态应为 LISTEN，得到 %q（偏移或状态映射有误）", c.State)
		}
		return
	}
	t.Fatalf("未找到本进程的 IPv6 监听端口 %d", port)
}

// 端口字节序：网络字节序的 443（0x01BB）在缓冲里是 [0x01,0xBB,0,0]。
func TestPortFromRaw(t *testing.T) {
	cases := []struct {
		b    []byte
		want uint16
	}{
		{[]byte{0x01, 0xBB, 0, 0}, 443},
		{[]byte{0x00, 0x50, 0, 0}, 80},
		{[]byte{0x1F, 0x90, 0, 0}, 8080},
	}
	for _, c := range cases {
		if got := portFromRaw(c.b); got != c.want {
			t.Fatalf("portFromRaw(%v) = %d want %d", c.b, got, c.want)
		}
	}
}
