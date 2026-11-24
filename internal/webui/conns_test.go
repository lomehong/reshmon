//go:build windows

package webui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lomehong/reshmon/internal/model"
)

// 回归：端口必须按数值排序。此前按地址字符串排序，445 被排到 15100 之后
// （真实输出：135、15100、15101、39894、445）。可接入的端点（TCP 监听 /
// UDP 绑定）在前，其后是已建立的连接，端口 0 的临时套接字放最后；同端口
// 按协议、本地 IP。
func TestConnRowsSortedByNumericPort(t *testing.T) {
	owners := map[uint32]connOwner{
		10: {proc: "svchost.exe", agent: "系统服务"},
		20: {proc: "app.exe", agent: "ZCode"},
	}
	conns := []model.ConnStat{
		{Proto: "UDP6", Local: "[::]:0", PID: 10}, // 临时套接字：端口 0
		{Proto: "TCP", Local: "0.0.0.0:15101", State: "LISTEN", PID: 20},
		{Proto: "TCP", Local: "127.0.0.1:52000", Remote: "1.2.3.4:443", State: "ESTAB", PID: 20},
		{Proto: "TCP", Local: "0.0.0.0:445", State: "LISTEN", PID: 10},
		{Proto: "UDP", Local: "0.0.0.0:5353", PID: 10},
		{Proto: "TCP6", Local: "[::]:135", State: "LISTEN", PID: 10},
		{Proto: "TCP", Local: "0.0.0.0:135", State: "LISTEN", PID: 10},
		{Proto: "TCP", Local: "0.0.0.0:15100", State: "LISTEN", PID: 20},
	}
	rows := buildConnRows(conns, owners, "", "")
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Proto + " " + r.Local
	}
	want := strings.Join([]string{
		"TCP 0.0.0.0:135", "TCP6 [::]:135", // 同端口：TCP 在 TCP6 前
		"TCP 0.0.0.0:445", "UDP 0.0.0.0:5353", // 组内纯按端口数值：5353 在 15100 之前
		"TCP 0.0.0.0:15100", "TCP 0.0.0.0:15101",
		"TCP 127.0.0.1:52000", // 已建立的连接在可接入端点之后
		"UDP6 [::]:0",         // 端口 0 的临时套接字最后：数值最小但不能挤在最前
	}, " | ")
	if strings.Join(got, " | ") != want {
		t.Fatalf("端点排序错误:\n got %s\nwant %s", strings.Join(got, " | "), want)
	}

	// 归属：进程名 + Agent 桶
	byLocal := map[string]connRow{}
	for _, r := range rows {
		byLocal[r.Local] = r
	}
	if r := byLocal["0.0.0.0:445"]; r.Proc != "svchost.exe" || r.Agent != "系统服务" {
		t.Fatalf("归属错误: %+v", r)
	}
	if r := byLocal["0.0.0.0:15101"]; r.Proc != "app.exe" || r.Agent != "ZCode" {
		t.Fatalf("归属错误: %+v", r)
	}
}

// 过滤：port 匹配本地或对端端口；pid 按 PID。
func TestConnRowsFilter(t *testing.T) {
	conns := []model.ConnStat{
		{Proto: "TCP", Local: "127.0.0.1:52000", Remote: "1.2.3.4:443", State: "ESTAB", PID: 20},
		{Proto: "TCP", Local: "0.0.0.0:445", State: "LISTEN", PID: 10},
		{Proto: "TCP", Local: "0.0.0.0:135", State: "LISTEN", PID: 10},
	}
	if n := len(buildConnRows(conns, nil, "443", "")); n != 1 {
		t.Fatalf("按对端端口 443 过滤应得 1 条，得到 %d", n)
	}
	if n := len(buildConnRows(conns, nil, "135", "")); n != 1 {
		t.Fatalf("按本地端口 135 过滤应得 1 条，得到 %d", n)
	}
	if n := len(buildConnRows(conns, nil, "", "10")); n != 2 {
		t.Fatalf("按 PID 10 过滤应得 2 条，得到 %d", n)
	}
	if n := len(buildConnRows(conns, nil, "9", "")); n != 0 {
		t.Fatalf("无人使用的端口应得 0 条，得到 %d", n)
	}
	// 空过滤器 = 不过滤
	if n := len(buildConnRows(conns, nil, "", "")); n != 3 {
		t.Fatalf("无过滤条件应返回全部，得到 %d", n)
	}
	// 过滤与归属互不影响：PID 不在归属表里时字段为空，不 panic
	rows := buildConnRows(conns, nil, "", "999999")
	if len(rows) != 0 {
		t.Fatalf("不存在的 PID 不应有结果，得到 %d", len(rows))
	}
}

type connsResp struct {
	Conns []connRow `json:"conns"`
	Total int       `json:"total"`
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求失败 %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
	}
	return resp.StatusCode
}

// 接口层：真实枚举。校验排序不变式，并用自建监听端口验证"端口→进程→归属"
// 这条链路（PID 必须是本测试进程，进程名必须来自采样快照）。
func TestConnsEndpoint(t *testing.T) {
	a := newTestApp(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法创建监听端口: %v", err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	srv := httptest.NewServer(Handler(a))
	defer srv.Close()

	self := uint32(os.Getpid())
	local := "127.0.0.1:" + port
	// 监听端口在快照之后才创建，归属要等下一拍；最多等 4 秒
	deadline := time.Now().Add(4 * time.Second)
	var found *connRow
	for time.Now().Before(deadline) {
		var r connsResp
		if code := getJSON(t, srv.URL+"/api/conns", &r); code != http.StatusOK {
			t.Fatalf("/api/conns 应 200: %d", code)
		}
		// 排序不变式：分组不回退，组内端口单调不减
		lastGroup, lastPort := 0, -1
		for _, c := range r.Conns {
			g := connGroup(c)
			if g < lastGroup {
				t.Fatalf("可接入端点应排在连接之前: %+v 出现在连接之后", c)
			}
			_, p := splitEndpoint(c.Local)
			if g == lastGroup && p < lastPort {
				t.Fatalf("同一组内端口应单调不减: %d 出现在 %d 之后（%+v）", p, lastPort, c)
			}
			lastGroup, lastPort = g, p
			if c.Proto == "TCP" && c.Local == local && c.PID == self && c.Proc != "" {
				cc := c
				found = &cc
			}
		}
		if found != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("未在 /api/conns 中找到本进程的监听端口（含进程名）")
	}
	if found.State != "LISTEN" || found.Remote != "" {
		t.Fatalf("监听端点字段错误: %+v", *found)
	}
	if !strings.HasSuffix(strings.ToLower(found.Proc), ".exe") {
		t.Fatalf("进程名应来自采样快照（可执行文件名），得到 %q", found.Proc)
	}

	// 按端口过滤：结果里每一行的本地或对端端口都等于该端口
	var f connsResp
	if code := getJSON(t, srv.URL+"/api/conns?port="+port, &f); code != http.StatusOK {
		t.Fatalf("按端口过滤应 200: %d", code)
	}
	if len(f.Conns) == 0 {
		t.Fatal("按自建端口过滤应有结果")
	}
	for _, c := range f.Conns {
		if !connPortMatches(model.ConnStat{Local: c.Local, Remote: c.Remote}, port) {
			t.Fatalf("过滤结果里出现了不匹配的端点: %+v", c)
		}
	}
	// 按 PID 过滤：全部是本进程
	var byPid connsResp
	if code := getJSON(t, srv.URL+"/api/conns?pid="+strconv.Itoa(int(self)), &byPid); code != http.StatusOK {
		t.Fatalf("按 PID 过滤应 200: %d", code)
	}
	if len(byPid.Conns) == 0 {
		t.Fatal("按本进程 PID 过滤应有结果")
	}
	for _, c := range byPid.Conns {
		if c.PID != self {
			t.Fatalf("按 PID 过滤结果混入了别的进程: %+v", c)
		}
	}
}

type atResp struct {
	Blind   bool   `json:"blind"`
	Reason  string `json:"reason"`
	DeltaMs int64  `json:"deltaMs"`
	Slice   *struct {
		T     int64 `json:"t"`
		Procs []struct {
			PID   uint32 `json:"pid"`
			Name  string `json:"n"`
			Agent string `json:"a"`
		} `json:"procs"`
	} `json:"slice"`
	Agents []struct {
		Name string `json:"n"`
	} `json:"agents"`
}

// 接口层：/api/at 参数校验、可回溯路径、盲区路径。
func TestAtEndpoint(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(Handler(a))
	defer srv.Close()

	for _, q := range []string{"", "?t=abc", "?t=0", "?t=-5"} {
		if code := getJSON(t, srv.URL+"/api/at"+q, nil); code != http.StatusBadRequest {
			t.Fatalf("/api/at%s 应 400，得到 %d", q, code)
		}
	}

	// 可回溯：启动后首份概览立即存档，当前时刻应返回切片（最多等 4 秒）
	deadline := time.Now().Add(4 * time.Second)
	var ok atResp
	for time.Now().Before(deadline) {
		var r atResp
		if code := getJSON(t, srv.URL+"/api/at?t="+strconv.FormatInt(time.Now().UnixMilli(), 10), &r); code != http.StatusOK {
			t.Fatalf("/api/at 应 200: %d", code)
		}
		if !r.Blind && r.Slice != nil {
			ok = r
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ok.Slice == nil {
		t.Fatal("当前时刻应可回溯，一直返回盲区")
	}
	if len(ok.Slice.Procs) == 0 {
		t.Fatal("切片应包含进程")
	}
	// 间距抽稀后概览最久约 6 秒一份，偏差不应超过 10 秒
	if ok.DeltaMs < -10000 || ok.DeltaMs > 10000 {
		t.Fatalf("当前时刻的切片偏差过大: %d ms", ok.DeltaMs)
	}
	// Agent 账本来自总量缓冲：进程有归属时账本不应为空
	if len(ok.Agents) == 0 {
		t.Fatal("应带该时刻的 Agent 账本")
	}

	// 盲区：3 小时前超出内存保留范围，必须如实说明而不是返回空切片
	var far atResp
	if code := getJSON(t, srv.URL+"/api/at?t="+strconv.FormatInt(time.Now().Add(-3*time.Hour).UnixMilli(), 10), &far); code != http.StatusOK {
		t.Fatalf("/api/at 应 200: %d", code)
	}
	if !far.Blind || far.Reason == "" || far.Slice != nil {
		t.Fatalf("超出保留范围应返回盲区及原因，且不带切片: %+v", far)
	}
}
