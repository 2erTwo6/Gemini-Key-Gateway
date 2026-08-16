//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// restartProcess 重新执行当前可执行文件，替换当前进程（PID 不变）。
// 适用于 Docker 容器 PID 1 / systemd / 手工运行等场景；仅在出错时返回。
func restartProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("exec self: %w", err)
	}
	return nil
}
