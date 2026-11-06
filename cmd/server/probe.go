package main

// 引擎加载的子进程隔离。
//
// 背景：sherpa-onnx 是 C++ 实现，模型文件损坏时 throw Ort::Exception，
// C++ 运行时直接 abort()。这个 abort 发生在 cgo 调用里，Go 的 recover 接不住
// （表现为 "signal arrived during cgo execution" + SIGABRT），整个服务进程会死。
//
// 后台的「重载引擎」允许用户随意改模型路径，一旦填错就把服务打死并进入重启循环，
// 这是不可接受的。所以加载前先在子进程里试一遍：子进程 abort 只会杀掉自己，
// 父进程拿到非零退出码，保留原引擎继续服务。
//
// 代价：探测要完整加载一遍模型（实测 ~7s，光速缓存后更快），
// 所以用「上次探测通过的指纹」做记忆——路径没变就跳过，路径一变立刻重探。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"xiaoai-go/internal/server/audio"
)

// probeEngineFlag 子进程模式参数。用参数而不是环境变量，便于 ps 上直接看出来。
const probeEngineFlag = "-probe-engine"

// probeTimeout 给模型加载留足时间（大模型 + 慢盘可能要几十秒）。
const probeTimeout = 3 * time.Minute

// probeStickyFile 记录「上次成功加载过的模型配置指纹」的文件名。
const probeStickyFile = ".engine_ok"

// runEngineProbeIfRequested 处理子进程探测模式，返回 true 表示已处理（调用方应直接退出）。
// 必须在 flag.Parse() 之前调用：探测参数不是标准 flag。
func runEngineProbeIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != probeEngineFlag {
		return false
	}
	// 配置从 stdin 读：避免命令行长度与转义问题
	var cfg audio.SherpaConfig
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "读取探测配置失败: %v\n", err)
		os.Exit(2)
	}
	eng, err := audio.NewSherpaEngine(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// 显式关掉：探测进程马上退出，但让 C++ 侧析构走完整路径，
	// 顺便也验证 Close 本身不会崩。
	_ = eng.Close()
	fmt.Printf("ok tts_rate=%d\n", eng.TTSRate())
	os.Exit(0)
	return true
}

// engineFingerprint 只覆盖「决定能不能加载」的字段——也就是模型路径。
// 阈值、语速、线程数改了不需要重新探测（它们不影响加载成败）。
func engineFingerprint(cfg audio.SherpaConfig) string {
	key := struct {
		ModelDir, VADModel, ASRDir                           string
		TTSModel, TTSTokens, TTSLexicon, TTSDataDir, TTSDict string
		KWSEnc, KWSDec, KWSJoin, KWSTok, KWSBpe, KWSKeyFile  string
	}{
		cfg.ModelDir, cfg.VADModel, cfg.ASRDir,
		cfg.TTSModel, cfg.TTSTokens, cfg.TTSLexicon, cfg.TTSDataDir, cfg.TTSDictDir,
		cfg.KWSEncoder, cfg.KWSDecoder, cfg.KWSJoiner, cfg.KWSTokens, cfg.KWSBpeModel, cfg.KeywordsFile,
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", key)))
	return hex.EncodeToString(sum[:16])
}

// probeEngine 在子进程里试加载一次模型，返回本次配置的指纹。
//   - 返回 "" 表示跳过了探测（指纹相同或显式关闭），调用方不必记录
//   - 返回非空指纹表示探测通过，加载成功后应调用 markEngineOK 记住它
func (s *server) probeEngine(cfg audio.SherpaConfig) (string, error) {
	if os.Getenv("XIAOAI_SKIP_PROBE") == "1" {
		return "", nil
	}
	fp := engineFingerprint(cfg)
	if s.engineFingerprintOK(fp) {
		return "", nil
	}

	exe, err := os.Executable()
	if err != nil {
		// 拿不到自身路径就放行：探测只是保险，不该反过来挡住正常加载
		return "", nil
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, probeEngineFlag)
	cmd.Stdin = bytes.NewReader(blob)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return fp, nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("模型加载自检超时（%s），请检查模型是否放在网络盘上或文件过大", probeTimeout)
	}

	// 被信号杀掉（abort / segv）比普通退出码更能说明问题
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() < 0 {
		return "", fmt.Errorf("模型加载时子进程崩溃（模型类型与推理器不匹配，或文件损坏）%s", reasonOf(out))
	}
	return "", fmt.Errorf("模型加载自检失败%s", reasonOf(out))
}

// reasonOf 从子进程输出里挑出「真正有用的那一句」。
// 直接回原始输出会把 onnxruntime 的 cpuid 警告和 C++ 崩溃时的 Go 调用栈
// 一起塞进接口返回值，前端没法看，所以这里只提取根因。
func reasonOf(out []byte) string {
	var reason string
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		// 噪声：运行时环境探测、崩溃时的信号信息与调用栈
		case strings.Contains(line, "cpuid_info warning"):
			continue
		case strings.HasPrefix(line, "SIGABRT"), strings.HasPrefix(line, "SIGSEGV"),
			strings.HasPrefix(line, "PC="), strings.HasPrefix(line, "signal arrived"),
			strings.HasPrefix(line, "goroutine"), strings.HasPrefix(line, "fault "),
			strings.HasPrefix(line, "runtime/"), strings.HasPrefix(line, "exit status"):
			continue
		// C++ 抛出异常时真正的错误内容在这里
		case strings.Contains(line, "what(): "):
			return "：" + clip(after(line, "what(): "), 180)
		}
		if reason == "" && !strings.Contains(line, "terminate called") {
			reason = line
		}
	}
	if reason == "" {
		return ""
	}
	return "：" + clip(reason, 180)
}

func after(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return strings.TrimSpace(s[i+len(sep):])
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stickyPath 返回指纹文件位置，与配置文件放在同一目录。
func (s *server) stickyPath() string {
	dir := filepath.Dir(*flagSettings)
	if dir == "" || dir == "." {
		return probeStickyFile
	}
	return filepath.Join(dir, probeStickyFile)
}

// engineFingerprintOK 判断这套模型配置上次是否已经成功加载过。
func (s *server) engineFingerprintOK(fp string) bool {
	b, err := os.ReadFile(s.stickyPath())
	return err == nil && strings.TrimSpace(string(b)) == fp
}

// markEngineOK 记住「这套配置能加载」，下次启动就不必再探测。
func (s *server) markEngineOK(fp string) {
	if fp == "" {
		return
	}
	if err := os.WriteFile(s.stickyPath(), []byte(fp+"\n"), 0o644); err != nil {
		// 写不进去只是下次多花几秒探测，不影响功能
		return
	}
}
