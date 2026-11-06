//go:build !linux

package main

import "os/exec"

// 本机（macOS）开发时的降级实现，只保证能编译与跑通 fakeclient 链路。

func modelName() string { return "dev" }

func firmwareVersion() string { return "dev" }

func diskFreeMB(path string) int64 { return 0 }

// defaultWakePath 是非 Linux（本机调试）下的默认位置。
const defaultWakePath = "wakewords.txt"

// 本机开发不涉及真实播放器，进程组控制是空实现。

func startNewProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {}
