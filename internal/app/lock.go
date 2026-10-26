//go:build windows

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lomehong/reshmon/internal/syswin"
)

// dirLock 数据目录写互斥。两个实例写同一目录会互相破坏：轮转时一方
// 删除另一方正以 O_APPEND 打开的 raw 文件（Windows 允许删除已打开文件，
// 另一方的后续写入全部蒸发），且 totals 交错重复。锁文件记录持有者 PID，
// 启动时发现持有者已退出则视为陈旧锁接管。
type dirLock struct {
	f    *os.File
	path string
}

func lockDataDir(dir string) (*dirLock, error) {
	path := filepath.Join(dir, "reshmon.lock")
	if b, err := os.ReadFile(path); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && syswin.PidAlive(uint32(pid)) {
			return nil, fmt.Errorf("数据目录 %s 正被另一个 reshmon 实例使用（pid %d）；确认其已退出后删除 %s 重试", dir, pid, path)
		}
		_ = os.Remove(path) // 陈旧锁：持有者进程已不存在
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("数据目录锁创建失败: %w", err)
	}
	if _, werr := f.WriteString(strconv.Itoa(os.Getpid())); werr != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, werr
	}
	return &dirLock{f: f, path: path}, nil
}

func (l *dirLock) release() {
	if l == nil {
		return
	}
	l.f.Close()
	_ = os.Remove(l.path)
}
