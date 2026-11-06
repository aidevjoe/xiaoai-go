package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ubus 的真实返回长这样（斜杠被 JSON 转义）：
// {"code":0,"info":"{ \"path\": \"\/tmp\/tts\/tts_xxxx.mp3\" }"}
func TestParseTTSPath(t *testing.T) {
	raw := "{\n\t\"code\": 0,\n\t\"info\": \"{ \\\"path\\\": \\\"\\/tmp\\/tts\\/tts_46ea5c32_1793895279_367857.mp3\\\" }\"\n}"
	got := parseTTSPath(raw)
	want := "/tmp/tts/tts_46ea5c32_1793895279_367857.mp3"
	if got != want {
		t.Fatalf("parseTTSPath = %q，期望 %q", got, want)
	}
}

func TestParseTTSPath_Missing(t *testing.T) {
	if got := parseTTSPath(`{"code":1}`); got != "" {
		t.Fatalf("没有 path 时应返回空串，实际 %q", got)
	}
}

// 短句/一段流式回答才缓存；超长文本不缓存（内容不重复，缓存只会白占内存盘）。
func TestTTSCachePath(t *testing.T) {
	if p := ttsCachePath("我在"); p == "" || !strings.HasPrefix(p, ttsCacheDir) {
		t.Fatalf("短句应生成缓存路径，实际 %q", p)
	}
	long := strings.Repeat("长", ttsCacheMaxRunes+1)
	if p := ttsCachePath(long); p != "" {
		t.Fatalf("超过 %d 字不该缓存，实际 %q", ttsCacheMaxRunes, p)
	}
	// 同一文本两次必须得到同一路径（缓存的全部意义）
	if ttsCachePath("我在") != ttsCachePath("我在") {
		t.Fatal("同一文本的缓存路径必须稳定")
	}
	// 不同文本不能撞
	if ttsCachePath("我在") == ttsCachePath("我在呢") {
		t.Fatal("不同文本不应命中同一缓存文件")
	}
}

// 命中判定：文件太小（空壳）不算命中。
func TestCachedTTS(t *testing.T) {
	dir := t.TempDir()
	text := "测试命中"
	if _, ok := cachedTTS(text); ok {
		t.Fatal("目录里没有文件不该命中")
	}
	p := filepath.Join(dir, "x.mp3")
	if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	// 把缓存路径指到临时目录里验证"大小门槛"
	real := ttsCachePath(text)
	if real == "" {
		t.Fatal("短句应有缓存路径")
	}
	_ = os.MkdirAll(filepath.Dir(real), 0o755)
	_ = os.WriteFile(real, make([]byte, 100), 0o644) // 太小
	if _, ok := cachedTTS(text); ok {
		t.Fatal("过小的文件不该算命中")
	}
	_ = os.WriteFile(real, make([]byte, 4096), 0o644)
	if got, ok := cachedTTS(text); !ok || got != real {
		t.Fatalf("正常大小的文件应命中，得到 %q ok=%v", got, ok)
	}
	_ = os.Remove(real)
}

// ubus 给的路径要真的是 /tmp 下一个像样的文件才拿去播。
// 这段字符串来自设备侧另一个进程，格式一变就可能带出奇怪的东西。
func TestValidateTTSPath(t *testing.T) {
	old := ttsPathPrefix
	ttsPathPrefix = t.TempDir() + "/"
	defer func() { ttsPathPrefix = old }()

	good := ttsPathPrefix + "ok.mp3"
	if err := os.WriteFile(good, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if !validateTTSPath(good) {
		t.Fatal("正常的音频文件应通过")
	}

	small := ttsPathPrefix + "tiny.mp3"
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if validateTTSPath(small) {
		t.Fatal("空壳文件不该通过（说明没合成成功）")
	}

	if validateTTSPath(ttsPathPrefix + "不存在.mp3") {
		t.Fatal("不存在的文件不该通过")
	}
	if validateTTSPath("/etc/passwd") {
		t.Fatal("目录外路径不该通过")
	}
	if validateTTSPath("relative/path.mp3") {
		t.Fatal("相对路径不该通过")
	}
	if validateTTSPath("") {
		t.Fatal("空串不该通过")
	}
}
