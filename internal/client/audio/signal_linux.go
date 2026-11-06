//go:build linux

package audio

import (
	"os"
	"syscall"
)

// termSignal 返回优雅停止信号。音箱是 Linux（busybox），用 SIGTERM 让 aplay 排空后退出。
func termSignal() os.Signal { return syscall.SIGTERM }
