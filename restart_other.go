//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix)

package main

import "fmt"

// restartProcess 在不支持 syscall.Exec 的平台上无法自重启。
func restartProcess() error {
	return fmt.Errorf("process restart is not supported on this platform")
}
