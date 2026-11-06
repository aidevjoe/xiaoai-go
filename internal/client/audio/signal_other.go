//go:build !linux

package audio

import (
	"os"
	"syscall"
)

// termSignal 在非 Linux（本机开发用 macOS）上同样返回 SIGTERM。
func termSignal() os.Signal { return syscall.SIGTERM }
