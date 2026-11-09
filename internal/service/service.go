//go:build windows

// Package service 把 reshmon 承载为 Windows 服务：serve 在服务环境下
// 自动进入 SCM 生命周期（停止请求→优雅关停），并提供 install/remove/
// start/stop/status 管理命令。依赖仅 golang.org/x/sys/windows/svc，
// 维持零第三方运行时依赖。
//
// 为什么不用计划任务：服务有崩溃自动恢复（SCM 重启动作）、登录前即可
// 运行、随系统统一启停（services.msc 可见）；此前 ONLOGON 计划任务
// 三者皆无。
package service

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/lomehong/reshmon/internal/app"
	"github.com/lomehong/reshmon/internal/webui"
)

// ServiceName SCM 注册名。
const ServiceName = "reshmon"

type serveHandler struct {
	cfg  app.Config
	addr string
}

// Execute 实现 svc.Handler：启动 App+HTTP，SCM 停止请求时优雅收尾。
func (h *serveHandler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.StartPending}

	a, err := app.Start(h.cfg)
	if err != nil {
		// 非零退出码：SCM 标记启动失败并按恢复动作重试
		return false, 1
	}

	stopCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- webui.ServeUntil(a, h.addr, stopCh) }()
	s <- svc.Status{State: svc.Running, Accepts: accepts}

	stopping := false
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			s <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			stopping = true
		}
		if stopping {
			break
		}
	}

	s <- svc.Status{State: svc.StopPending}
	close(stopCh)
	select { // 给 HTTP 收尾最多 10s
	case <-errCh:
	case <-time.After(10 * time.Second):
	}
	a.Close()
	s <- svc.Status{State: svc.Stopped}
	return false, 0
}

// RunService 服务入口（serve 检测到服务环境时调用）。阻塞至 SCM 停止。
func RunService(cfg app.Config, addr string) error {
	is, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("探测服务环境失败: %w", err)
	}
	if !is {
		return fmt.Errorf("非服务环境（交互运行请直接用 serve）")
	}
	return svc.Run(ServiceName, &serveHandler{cfg: cfg, addr: addr})
}

// ---- 管理命令（install/remove/start/stop/status，需管理员） ----

// binPath 安装时固化的服务命令行：绝对 exe 路径 + serve 参数。
// 数据目录固化为调用者解析出的绝对路径——服务默认以 LocalSystem 运行，
// 其 %LOCALAPPDATA% 指向系统配置文件目录，不固化会让数据"搬家"。
func binPath(cfg app.Config, addr string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("解析自身路径失败: %w", err)
	}
	args := []string{exe, "serve", "-addr", addr}
	if cfg.DataDir != "" {
		args = append(args, "-data", cfg.DataDir)
	}
	return args, nil
}

// Install 注册服务（自动启动 + 延迟启动避免拖慢开机 + 崩溃自动恢复）。
func Install(cfg app.Config, addr string) error {
	if is, _ := svc.IsWindowsService(); is {
		return fmt.Errorf("服务进程内不能执行 install")
	}
	args, err := binPath(cfg, addr)
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(ServiceName); err == nil {
		s.Close()
		return fmt.Errorf("服务 %s 已存在（先执行 reshmon service remove）", ServiceName)
	}
	s, err := m.CreateService(ServiceName, args[0], mgr.Config{
		DisplayName:      "reshmon 资源健康监护",
		Description:      "CPU/内存/磁盘/GPU 采集 + Agent 归因 + Web 面板（" + addr + "）。崩溃由 SCM 自动重启。",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true, // 延迟自启，不抢开机关键路径
	}, args[1:]...)
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	defer s.Close()
	// 崩溃恢复：5s/30s/60s 三档递增重启；一天内无失败则重置计数
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)
	return nil
}

func openService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("连接服务管理器失败（需要管理员权限）: %w", err)
	}
	s, err := m.OpenService(ServiceName)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("服务 %s 未安装（先执行 reshmon service install）", ServiceName)
	}
	return m, s, nil
}

// Remove 停止并删除服务。
func Remove() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer func() { s.Close(); m.Disconnect() }()
	if st, _ := s.Query(); st.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		waitStopped(s)
	}
	return s.Delete()
}

// Start 启动服务。
func Start() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer func() { s.Close(); m.Disconnect() }()
	return s.Start()
}

// Stop 停止服务（保留注册）。
func Stop() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer func() { s.Close(); m.Disconnect() }()
	if st, _ := s.Query(); st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	waitStopped(s)
	return nil
}

// Status 返回可读的服务状态。
func Status() (string, error) {
	m, s, err := openService()
	if err != nil {
		return "", err
	}
	defer func() { s.Close(); m.Disconnect() }()
	st, err := s.Query()
	if err != nil {
		return "", err
	}
	switch st.State {
	case svc.Stopped:
		return "已停止", nil
	case svc.Running:
		return "运行中 (pid " + fmt.Sprint(st.ProcessId) + ")", nil
	case svc.StartPending:
		return "启动中…", nil
	case svc.StopPending:
		return "停止中…", nil
	default:
		return fmt.Sprintf("状态 %d", st.State), nil
	}
}

func waitStopped(s *mgr.Service) {
	for i := 0; i < 100; i++ { // 最长 ~10s
		if st, err := s.Query(); err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
