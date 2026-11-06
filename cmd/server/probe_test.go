package main

import (
	"strings"
	"testing"

	"xiaoai-go/internal/server/audio"
)

// sherpaCfgFixture 一份典型的模型配置，供指纹测试使用。
func sherpaCfgFixture() audio.SherpaConfig {
	return audio.SherpaConfig{
		ModelDir:      "/models",
		VADModel:      "/models/silero_vad.onnx",
		ASRDir:        "/models/sense-voice",
		TTSModel:      "/models/vits-icefall-zh-aishell3/model.onnx",
		Threads:       4,
		VADMinSilence: 0.6,
	}
}

// 子进程崩溃时原始输出里混着 onnxruntime 的环境警告和 C++ abort 时的 Go 调用栈，
// 接口返回值必须只保留根因，否则前端与日志都没法读。
func TestReasonOf_ExtractsOrtException(t *testing.T) {
	out := []byte(`onnxruntime cpuid_info warning: Unknown CPU vendor. cpuinfo_vendor value: 0
terminate called after throwing an instance of 'Ort::Exception'
  what():  Load model from /models/bad/model.onnx failed:Protobuf parsing failed.
SIGABRT: abort
PC=0xfe5d12293c18 m=0 sigcode=18446744073709551610
signal arrived during cgo execution

goroutine 1 gp=0x400011ddc0 m=8 mp=0x4000077008 [syscall]:
runtime.cgocall(0x6fcb00, 0x400016bdb8)
`)
	got := reasonOf(out)
	if !strings.Contains(got, "Protobuf parsing failed") {
		t.Fatalf("没提取到根因: %q", got)
	}
	for _, noise := range []string{"cpuid_info", "SIGABRT", "goroutine", "runtime.cgocall", "PC=0x"} {
		if strings.Contains(got, noise) {
			t.Fatalf("输出里应剔除噪声 %q，实际: %q", noise, got)
		}
	}
}

func TestReasonOf_FallsBackToFirstRealLine(t *testing.T) {
	out := []byte("onnxruntime cpuid_info warning: Unknown CPU vendor\n" +
		"audio: ASR 目录 /models/x 里找不到 model.onnx\n" +
		"goroutine 1 [running]:\n")
	got := reasonOf(out)
	if !strings.Contains(got, "找不到 model.onnx") {
		t.Fatalf("应回退到第一条真实信息，得到: %q", got)
	}
}

func TestReasonOf_EmptyWhenOnlyNoise(t *testing.T) {
	if got := reasonOf([]byte("onnxruntime cpuid_info warning: x\nSIGABRT: abort\n")); got != "" {
		t.Fatalf("全是噪声时应返回空串，得到: %q", got)
	}
}

// 指纹只覆盖模型路径：改阈值/语速不该触发重新探测（否则用户每调一次都要等 7 秒）。
func TestEngineFingerprint_IgnoresRuntimeParams(t *testing.T) {
	base := sherpaCfgFixture()
	a := engineFingerprint(base)

	b := base
	b.Threads = 8
	b.VADThreshold = 0.9
	b.VADMinSpeech = 0.9
	if engineFingerprint(b) != a {
		t.Fatal("线程数与阈值不应影响指纹")
	}

	c := base
	c.TTSModel = base.TTSModel + ".other"
	if engineFingerprint(c) == a {
		t.Fatal("换 TTS 模型必须改变指纹，否则不会重新探测")
	}
}
