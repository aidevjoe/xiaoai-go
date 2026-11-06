package audio

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTTS 是最小的合成实现，用来证明"接一个供应商只需要实现接口 + 注册"。
type fakeTTS struct {
	text string
	rate int
}

func (f *fakeTTS) Synthesize(_ context.Context, text string) ([]byte, error) {
	f.text = text
	return []byte{1, 2, 3, 4}, nil
}
func (f *fakeTTS) TTSRate() int   { return f.rate }
func (f *fakeTTS) Status() string { return "fake" }
func (f *fakeTTS) Close() error   { return nil }

type fakeASR struct{ out string }

func (f *fakeASR) Transcribe(context.Context, []byte) (string, error) { return f.out, nil }
func (f *fakeASR) Status() string                                     { return "fake" }
func (f *fakeASR) Close() error                                       { return nil }

// 注册一个名字只能用一次，所以测试里用各自唯一的名字，
// 避免和其它测试（或多次运行）撞车。
func TestRegistry_TTSProviderIsPluggable(t *testing.T) {
	RegisterTTS(TTSProvider{
		Name: "t-fake-1", Desc: "测试用合成器", Local: false,
		New: func(o TTSOptions) (Synthesizer, error) {
			if o.Provider != "t-fake-1" {
				t.Fatalf("Options.Provider = %q，应被填成注册名", o.Provider)
			}
			return &fakeTTS{rate: 22050}, nil
		},
	})

	if !HasTTSProvider("T-FAKE-1") { // 大小写应被归一
		t.Fatal("注册后 HasTTSProvider 应忽略大小写")
	}
	var found bool
	for _, p := range TTSProviders() {
		if p.Name == "t-fake-1" {
			found = true
			if p.Desc != "测试用合成器" || p.Local {
				t.Fatalf("候选信息不对: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("TTSProviders 没列出刚注册的供应商")
	}

	syn, err := NewSynthesizer("T-Fake-1", TTSOptions{})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	pcm, err := syn.Synthesize(context.Background(), "你好")
	if err != nil || len(pcm) == 0 {
		t.Fatalf("合成失败: %v", err)
	}
	if syn.(*fakeTTS).text != "你好" {
		t.Fatal("文本没传到合成器")
	}
	if syn.TTSRate() != 22050 {
		t.Fatalf("采样率 = %d", syn.TTSRate())
	}
}

func TestRegistry_UnknownProviderListsCandidates(t *testing.T) {
	_, err := NewSynthesizer("不存在的供应商", TTSOptions{})
	if err == nil {
		t.Fatal("未知供应商应报错")
	}
	// 错误信息要把可用名字列出来，否则用户只能靠猜
	if !strings.Contains(err.Error(), "none") {
		t.Fatalf("错误信息应列出可用供应商: %v", err)
	}

	if _, err := NewRecognizer("不存在的供应商", ASROptions{}); err == nil {
		t.Fatal("未知 ASR 供应商应报错")
	}
	if _, err := NewVoiceDetector("nope", VADOptions{}); err == nil {
		t.Fatal("未知 VAD 供应商应报错")
	}
	if _, err := NewKeywordDetector("nope", KWSOptions{}); err == nil {
		t.Fatal("未知 KWS 供应商应报错")
	}
}

func TestRegistry_NoneDisablesCapability(t *testing.T) {
	syn, err := NewSynthesizer("none", TTSOptions{})
	if err != nil {
		t.Fatalf("none 应该能创建: %v", err)
	}
	if _, err := syn.Synthesize(context.Background(), "hi"); !errors.Is(err, ErrCapability) {
		t.Fatalf("none 合成应返回 ErrCapability，实际 %v", err)
	}
	// 状态必须是 off 而不是伪装成模型名，后台一眼能看出是"关了"而不是"坏了"
	if syn.Status() != "off" {
		t.Fatalf("Status = %q", syn.Status())
	}
	// 采样率仍要给一个安全值，否则播放端会拿到 0
	if syn.TTSRate() != 24000 {
		t.Fatalf("TTSRate = %d", syn.TTSRate())
	}

	rec, err := NewRecognizer("none", ASROptions{})
	if err != nil {
		t.Fatalf("none ASR 应该能创建: %v", err)
	}
	if _, err := rec.Transcribe(context.Background(), []byte{1, 2}); !errors.Is(err, ErrCapability) {
		t.Fatalf("none 识别应返回 ErrCapability，实际 %v", err)
	}
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	RegisterTTS(TTSProvider{Name: "t-dup", New: func(TTSOptions) (Synthesizer, error) {
		return &fakeTTS{}, nil
	}})
	defer func() {
		if recover() == nil {
			t.Fatal("重名注册应 panic（这是代码结构错误，不该留到线上）")
		}
	}()
	RegisterTTS(TTSProvider{Name: "t-dup", New: func(TTSOptions) (Synthesizer, error) {
		return &fakeTTS{}, nil
	}})
}

// Bundle 是"零件可缺"的引擎：缺 TTS 不影响识别，缺 KWS 只是不唤醒。
func TestBundle_MissingPartsDegradeGracefully(t *testing.T) {
	b := NewBundle(BundleParts{ASR: &fakeASR{out: "你好"}})
	defer b.Close()

	if b.HasTTS() || b.HasKWS() || b.HasVAD() {
		t.Fatal("没提供的零件不该被认为是就绪的")
	}
	if !b.HasASR() {
		t.Fatal("ASR 应当就绪")
	}
	text, err := b.Transcribe(context.Background(), []byte{1, 2})
	if err != nil || text != "你好" {
		t.Fatalf("识别失败: %q %v", text, err)
	}
	if _, err := b.Synthesize(context.Background(), "hi"); !errors.Is(err, ErrCapability) {
		t.Fatalf("没有 TTS 时应返回 ErrCapability，实际 %v", err)
	}
	// KWS 缺失不能报错，否则每一帧都会刷日志
	if kw, err := b.DetectKeyword([]byte{1}); err != nil || kw != "" {
		t.Fatalf("没有 KWS 时应静默返回空，实际 %q %v", kw, err)
	}
	if b.TTSRate() != 24000 {
		t.Fatalf("没有 TTS 时采样率应给默认值，实际 %d", b.TTSRate())
	}
	if !strings.Contains(b.Status(), "VAD=未加载") {
		t.Fatalf("状态应写明未加载: %s", b.Status())
	}
}

// 换 TTS 时语速要带过去，否则后台调过的语速会被"重载模型"悄悄重置成 1.0。
func TestBundle_SpeedSurvivesPartSwap(t *testing.T) {
	first := &fakeTTS{rate: 16000}
	b := NewBundle(BundleParts{TTS: first, Speed: 0.8})
	if got := b.Speed(); got != 0.8 {
		t.Fatalf("初始语速 = %v", got)
	}
	b.SetSpeed(1.3)
	if got := b.Speed(); got != 1.3 {
		t.Fatalf("设置后语速 = %v", got)
	}

	// 重新组装一个 Bundle（模拟换 TTS），把语速带过去
	b2 := NewBundle(BundleParts{TTS: first, Speed: b.Speed()})
	defer b2.Close()
	if got := b2.Speed(); got != 1.3 {
		t.Fatalf("换零件后语速丢了: %v", got)
	}
}

func TestBundle_CloseIsIdempotent(t *testing.T) {
	b := NewBundle(BundleParts{VAD: nil})
	if err := b.Close(); err != nil {
		t.Fatalf("Close 出错: %v", err)
	}
	if err := b.Close(); err != nil { // 第二次必须是安全的
		t.Fatalf("重复 Close 出错: %v", err)
	}
}
