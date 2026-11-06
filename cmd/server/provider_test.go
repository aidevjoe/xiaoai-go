package main

import (
	"testing"

	"xiaoai-go/internal/server/audio"
)

// 供应商解析要"配错也不至于让音箱哑掉"。
func TestResolveProvider(t *testing.T) {
	exists := func(name string) bool { return name == "sherpa" || name == "none" }

	cases := []struct {
		in   string
		want string
	}{
		{"", "sherpa"},       // 没配 → 用本地
		{"sherpa", "sherpa"}, // 明确指定
		{"SHERPA", "sherpa"}, // 大小写归一
		{"   ", "sherpa"},    // 空白当没配
		{"不存在的", "sherpa"},   // 本构建没装 → 回退
		{"none", "none"},     // 显式关闭要尊重
	}
	for _, c := range cases {
		if got := resolveProvider(c.in, "TTS", exists); got != c.want {
			t.Fatalf("resolveProvider(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 连 sherpa 都没有的构建（例如本机开发）：只能关闭，不能崩
	none := func(string) bool { return false }
	if got := resolveProvider("", "TTS", none); got != "none" {
		t.Fatalf("没有 sherpa 的构建应回退到 none，实际 %q", got)
	}
}

// 云端供应商创建失败时必须回退本地模型：宁可少一个功能，不能让助手哑掉。
func TestSynthesizerFallbackRegistration(t *testing.T) {
	// 注册一个永远失败的供应商，验证 Has* + 回退分支的前提条件成立
	audio.RegisterTTS(audio.TTSProvider{
		Name:  "always-fail-test",
		Local: false,
		New: func(audio.TTSOptions) (audio.Synthesizer, error) {
			return nil, errTestProvider
		},
	})
	if !audio.HasTTSProvider("always-fail-test") {
		t.Fatal("注册后应能被查到（回退判断依赖它）")
	}
	if audio.TTSProviderLocal("always-fail-test") {
		t.Fatal("云端供应商不该被当成需要本地模型")
	}
	if _, err := audio.NewSynthesizer("always-fail-test", audio.TTSOptions{}); err == nil {
		t.Fatal("应该返回错误，回退逻辑才有机会接管")
	}
}

var errTestProvider = &providerErr{}

type providerErr struct{}

func (*providerErr) Error() string { return "供应商不可用（测试用）" }
