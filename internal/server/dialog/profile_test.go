package dialog

import (
	"io"
	"log"
	"testing"
	"time"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
)

func f32(v float32) *float32 { return &v }
func i(v int) *int           { return &v }

func newOverrideManager() *Manager {
	m := NewManager(&agent.Registry{}, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	m.Speak = SpeakOptions{
		MaxSentenceRunes: 20, MaxSentences: 6, StripEmoji: true, DeviceTTS: false,
	}
	m.GlobalSpeed = 1.0
	m.Timeout = 20 * time.Second
	return m
}

// 档案没配 voice 时，必须完全跟随全局——不能因为"字段是零值"就把全局覆盖成 0。
func TestSpeakOptions_ProfileWithoutOverrideFollowsGlobal(t *testing.T) {
	m := newOverrideManager()
	opt := m.speakOptionsOf(&agent.Profile{ID: "a", Name: "管家"})
	if opt.MaxSentenceRunes != 20 || opt.MaxSentences != 6 || opt.DeviceTTS {
		t.Fatalf("没配 voice 时应完全跟全局: %+v", opt)
	}
	if got := m.SpeedOf(&agent.Profile{ID: "a"}); got != 1.0 {
		t.Fatalf("语速应跟全局，实际 %v", got)
	}
	if got := m.TimeoutOf(&agent.Profile{ID: "a"}); got != 20*time.Second {
		t.Fatalf("超时应跟全局，实际 %v", got)
	}
}

func TestSpeakOptions_ProfileOverrides(t *testing.T) {
	m := newOverrideManager()
	p := &agent.Profile{
		ID: "translate", Name: "翻译",
		Voice: &agent.VoiceOverride{
			Speed:            f32(0.85),
			MaxSentenceRunes: i(12),
			MaxSentences:     i(2),
			Backend:          "device",
			TimeoutSec:       i(8),
		},
	}
	opt := m.speakOptionsOf(p)
	if opt.MaxSentenceRunes != 12 {
		t.Fatalf("切句长度应被覆盖为 12，实际 %d", opt.MaxSentenceRunes)
	}
	if opt.MaxSentences != 2 {
		t.Fatalf("单轮句数应被覆盖为 2，实际 %d", opt.MaxSentences)
	}
	if !opt.DeviceTTS {
		t.Fatal("Backend=device 应打开设备原生 TTS")
	}
	if got := m.SpeedOf(p); got != 0.85 {
		t.Fatalf("语速覆盖失效，实际 %v", got)
	}
	if got := m.TimeoutOf(p); got != 8*time.Second {
		t.Fatalf("超时覆盖失效，实际 %v", got)
	}
}

// 档案显式写 max_sentences=0 意思是"不限"，不能被全局的 6 顶掉。
func TestSpeakOptions_ZeroMeansUnlimited(t *testing.T) {
	m := newOverrideManager()
	p := &agent.Profile{ID: "news", Voice: &agent.VoiceOverride{MaxSentences: i(0)}}
	if got := m.speakOptionsOf(p).MaxSentences; got != 0 {
		t.Fatalf("显式 0 应表示不限句数，实际 %d", got)
	}
}

// 语速是引擎级状态：换助手时必须能把全局语速恢复回来，
// 否则「翻译」助手调慢过之后，切回「管家」还是慢的。
func TestApplySpeed_RestoresGlobalAfterProfileOverride(t *testing.T) {
	m := newOverrideManager()
	slow := &agent.Profile{ID: "slow", Voice: &agent.VoiceOverride{Speed: f32(0.7)}}
	noprofile := &agent.Profile{ID: "normal"}

	m.ApplySpeed(m.SpeedOf(slow))
	m.ApplySpeed(m.SpeedOf(noprofile))

	// MockEngine 不实现 SpeedSetter 时这里只能验证调用不 panic；
	// 用能记录语速的引擎再验一次真实行为
	rec := &speedRecorder{Engine: audio.NewMockEngine(0.9, "x")}
	m.SetEngine(rec)
	m.ApplySpeed(m.SpeedOf(slow))
	if got := rec.speed; got != 0.7 {
		t.Fatalf("档案语速没落到引擎上: %v", got)
	}
	m.ApplySpeed(m.SpeedOf(noprofile))
	if got := rec.speed; got != 1.0 {
		t.Fatalf("切回没配语速的档案后应恢复全局 1.0，实际 %v", got)
	}
}

type speedRecorder struct {
	audio.Engine
	speed float32
}

func (s *speedRecorder) SetSpeed(v float32) { s.speed = v }

// 档案只写了音色、没写供应商：供应商必须继续跟全局（同一个账号、不同音色）。
func TestVoiceOf_ProfileVoiceOnlyKeepsGlobalProvider(t *testing.T) {
	m := newOverrideManager()
	m.GlobalVoice = audio.VoiceChoice{Provider: "doubao", Voice: "global-voice",
		Params: map[string]string{"api_key": "k"}}
	got := m.VoiceOf(&agent.Profile{ID: "p", TTSSpeaker: "zh_female_shuangkuaisisi_moon_bigtts"})
	if got.Provider != "doubao" {
		t.Fatalf("供应商应跟全局，实际 %q", got.Provider)
	}
	if got.Voice != "zh_female_shuangkuaisisi_moon_bigtts" {
		t.Fatalf("音色应被覆盖，实际 %q", got.Voice)
	}
	if got.Params["api_key"] != "k" {
		t.Fatalf("凭据应继承全局，实际 %v", got.Params)
	}
}

// 档案换了平台：必须带上自己的参数，不能继续用全局平台的凭据。
func TestVoiceOf_ProfileProviderSwitchUsesOwnParams(t *testing.T) {
	m := newOverrideManager()
	m.GlobalVoice = audio.VoiceChoice{Provider: "doubao", Params: map[string]string{"api_key": "k"}}
	got := m.VoiceOf(&agent.Profile{ID: "p", TTSProvider: "edge", TTSSpeaker: "zh-CN-XiaoxiaoNeural"})
	if got.Provider != "edge" || got.Voice != "zh-CN-XiaoxiaoNeural" {
		t.Fatalf("解析结果不对: %+v", got)
	}
	if len(got.Params) != 0 {
		t.Fatalf("不该带入豆包的参数: %v", got.Params)
	}
}

// 音色选择必须进 SpeakOptions，否则播放器根本不知道要换声音。
func TestSpeakOptions_CarriesVoice(t *testing.T) {
	m := newOverrideManager()
	m.GlobalVoice = audio.VoiceChoice{Provider: "edge", Voice: "zh-CN-YunxiNeural"}
	m.GlobalSpeed = 1.2
	opt := m.speakOptionsOf(&agent.Profile{ID: "p", Voice: &agent.VoiceOverride{Speed: f32(0.9)}})
	if opt.Voice.Provider != "edge" || opt.Voice.Voice != "zh-CN-YunxiNeural" {
		t.Fatalf("音色没带进 SpeakOptions: %+v", opt.Voice)
	}
	// 档案语速要覆盖全局，否则"翻译"助手调慢过之后切回去还是慢的
	if opt.Voice.Speed != 0.9 {
		t.Fatalf("档案语速没生效: %v", opt.Voice.Speed)
	}
}

// 固定应答缓存必须按音色分键：同一个"我在"换个助手不能念出上一个的声音。
func TestCacheKey_SeparatesVoices(t *testing.T) {
	a := cacheKey("我在", 1, "doubao|v1")
	b := cacheKey("我在", 1, "doubao|v2")
	if a == b {
		t.Fatal("不同音色必须命中不同的缓存键")
	}
	if a != cacheKey("我在", 1, "doubao|v1") {
		t.Fatal("同参数必须稳定")
	}
	// 换供应商也算换音色：同一个音色 ID 在不同平台是不同的声音
	if a == cacheKey("我在", 1, "edge|v1") {
		t.Fatal("不同供应商必须命中不同的缓存键")
	}
}
