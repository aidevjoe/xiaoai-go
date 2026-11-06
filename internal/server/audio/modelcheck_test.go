package audio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这个用例守的是一个真实事故：vits-melo-tts-zh_en/model.int8.onnx 是没下载的
// Git LFS 指针（133 字节），sherpa 加载时 C++ abort，把整个服务打死并进入重启循环。
// 修法是在交给 C++ 之前就拦下来，并且给出能照做的提示。
func TestValidateModelFiles_RejectsLFSPointer(t *testing.T) {
	dir := t.TempDir()
	vad := writeFile(t, dir, "silero_vad.onnx", 200<<10)
	lfs := writeFile(t, dir, "model.int8.onnx",
		0, "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 170429550\n")

	cfg := SherpaConfig{ModelDir: dir, VADModel: vad, TTSModel: lfs}
	err := ValidateModelFiles(cfg)
	if err == nil {
		t.Fatal("LFS 指针文件应该被拒绝，却通过了自检")
	}
	if !strings.Contains(err.Error(), "Git LFS") {
		t.Fatalf("错误信息没点明 LFS: %v", err)
	}
	if !strings.Contains(err.Error(), lfs) {
		t.Fatalf("错误信息没带具体路径: %v", err)
	}
}

func TestValidateModelFiles_RejectsTruncated(t *testing.T) {
	dir := t.TempDir()
	vad := writeFile(t, dir, "silero_vad.onnx", 200<<10)
	broken := writeFile(t, dir, "model.onnx", 100) // 下载中断的残片

	err := ValidateModelFiles(SherpaConfig{ModelDir: dir, VADModel: vad, TTSModel: broken})
	if err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("残片文件应报「不完整」，得到: %v", err)
	}
}

func TestValidateModelFiles_RejectsDirAsModel(t *testing.T) {
	dir := t.TempDir()
	vad := writeFile(t, dir, "silero_vad.onnx", 200<<10)
	sub := filepath.Join(dir, "vits")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	err := ValidateModelFiles(SherpaConfig{ModelDir: dir, VADModel: vad, TTSModel: sub})
	if err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("指向目录时应提示需要具体文件，得到: %v", err)
	}
}

func TestValidateModelFiles_AcceptsHealthy(t *testing.T) {
	dir := t.TempDir()
	vad := writeFile(t, dir, "silero_vad.onnx", 200<<10)
	ttsDir := filepath.Join(dir, "vits-icefall-zh-aishell3")
	if err := os.MkdirAll(ttsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tts := writeFile(t, ttsDir, "model.onnx", 1<<20)
	writeFile(t, ttsDir, "tokens.txt", 1<<10)

	cfg := SherpaConfig{ModelDir: dir, VADModel: vad, TTSModel: tts}
	if err := ValidateModelFiles(cfg); err != nil {
		t.Fatalf("正常模型不该报错: %v", err)
	}
}

// writeFile 生成指定大小的测试文件；extra 非空时作为文件内容（用于构造 LFS 指针）。
func writeFile(t *testing.T, dir, name string, size int, extra ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	var body []byte
	if len(extra) > 0 {
		body = []byte(extra[0])
	} else {
		body = make([]byte, size)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
