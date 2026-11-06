//go:build linux

package main

import (
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// modelName 读取机型。LX06 上有 micocfg_model 命令。
func modelName() string {
	for _, cmd := range []string{"micocfg_model"} {
		if out, err := exec.Command(cmd).Output(); err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				return s
			}
		}
	}
	return "LX06"
}

// firmwareVersion 读取系统版本。不同固件放的位置不一样，逐个尝试。
func firmwareVersion() string {
	if out, err := exec.Command("sh", "-c",
		`cat /etc/mico_version 2>/dev/null || grep -o 'Ver:[0-9.]*' /etc/os-release 2>/dev/null || echo unknown`,
	).Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return strings.TrimPrefix(s, "Ver:")
		}
	}
	return "unknown"
}

// diskFreeMB 返回指定分区的可用空间（MB）。
func diskFreeMB(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1024 * 1024)
}

// defaultWakePath 是唤醒词清单的默认位置。
//
// 刻意不用 sherpa 的 keywords.txt：那个文件是「token @显示名」格式，
// 覆盖了会让 KWS 加载失败。这里存一份纯文本清单，两边语义各自独立。
const defaultWakePath = "/data/xiaoai/wakewords.txt"

// startNewProcessGroup 让脚本在独立进程组里跑。
//
// 为什么必须这样：播放脚本是 `/usr/bin/mphelper pause; miplayer -f xxx`，
// sh 之下还挂着 miplayer。原来用 exec.CommandContext，取消时只杀 sh，
// miplayer 继续播——于是只能靠 `pkill -x miplayer` 兜底，而那会把用户
// 正在放的音乐一起掐掉。独立进程组之后，杀一整组就精确了。
func startNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 先礼后兵地终止整个进程组。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// 负 pid = 发给整个进程组（sh + miplayer + mphelper）
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return
	}
	// miplayer 偶尔不理 SIGTERM，补一刀
	time.Sleep(150 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
