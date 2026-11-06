package audio

// 能力拆分：把原来"一个 Engine 管四件事"拆成四个可替换的零件。
//
// 为什么要拆：
//   - 换一个 TTS 模型要重建整个引擎（ASR/VAD/KWS 一起重载，4~12 秒，
//     几百 MB 内存抖一下），因为它们是同一个结构体、同一把锁。
//   - 一把大锁串行化全部推理：Matcha 合成一句话要 4 秒，这 4 秒里
//     VAD 拿不到锁 → KWS 也拿不到 → 唤醒变迟钝。
//   - 想接一个云端 TTS（比如 edge-tts）无从下手：Engine 接口要求你
//     同时实现 VAD/ASR/KWS，哪怕这些你根本不提供。
//
// 拆完的好处：
//   - 各零件各有一把锁，TTS 合成期间 VAD/KWS 照常工作。
//   - 每个零件可以单独加载、单独失败：TTS 模型坏了，ASR 仍然能用。
//   - 零件由注册表提供（见 registry.go），接新供应商 = 新增一个文件。
//
// Engine 接口保留不变（Bundle 实现它），这样上层代码一处都不用改，
// 后续可以按需把调用点逐个改成直接用零件。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Synthesizer 是文本转语音的能力。
//
// 接入一个新的 TTS 只需要实现这个接口并注册进 registry：
//
//	audio.RegisterTTS(audio.TTSProvider{Name: "edge", New: newEdgeTTS})
type Synthesizer interface {
	// Synthesize 把一句文本合成 16bit/mono PCM（采样率见 TTSRate）。
	Synthesize(ctx context.Context, text string) ([]byte, error)
	// TTSRate 返回合成音频的采样率。
	TTSRate() int
	// Status 返回一句话描述（后台与日志用）。
	Status() string
	// Close 释放资源。
	Close() error
}

// Recognizer 是语音转文本的能力。
type Recognizer interface {
	// Transcribe 识别一整段音频（16k/S16LE/mono）。
	Transcribe(ctx context.Context, pcm []byte) (string, error)
	Status() string
	Close() error
}

// VoiceDetector 判断一帧是否人声（VAD）。
type VoiceDetector interface {
	SpeechProb(frame []byte) (float32, error)
	Status() string
	Close() error
}

// KeywordDetector 检测唤醒词（KWS），未命中返回空串。
type KeywordDetector interface {
	DetectKeyword(frame []byte) (string, error)
	Status() string
	Close() error
}

// 可选能力：语速可调（大部分本地模型可以，云端 API 由参数控制）。
type (
	// SpeedSetter 运行时调整语速（1.0 = 原速），不重建模型。
	SpeedSetter interface{ SetSpeed(speed float32) }
	// SpeedReader 读取当前语速（缓存键需要它：语速变了旧音频就作废）。
	SpeedReader interface{ Speed() float32 }
	// SpeakerSelector 支持选择音色（多说话人的模型 / 云端 TTS 的 voice 参数）。
	SpeakerSelector interface{ SetSpeaker(id int) }
)

// ErrCapability 表示某个零件没加载（用来区分"能力缺失"与"推理失败"）。
var ErrCapability = errors.New("audio: 能力未加载")

// Bundle 把四个零件拼成一个 Engine。
//
// 零件可以为 nil：缺哪个就哪个能力不可用，其它零件照常工作。
// 这正是"TTS 模型坏了不该让识别也废掉"的实现方式。
type Bundle struct {
	vad VoiceDetector
	asr Recognizer
	tts Synthesizer
	kws KeywordDetector

	// asrName/ttsName 只用于状态展示（"TTS=matcha(22050Hz)"）。
	asrName string
	ttsName string

	// 语速：Bundle 统一管，换 TTS 时把语速带过去，
	// 免得后台改过的语速因为重载模型被悄悄重置。
	speedMu sync.RWMutex
	speed   float32

	closeOnce sync.Once
}

// BundleParts 是构造 Bundle 的零件集合。
type BundleParts struct {
	VAD VoiceDetector
	ASR Recognizer
	TTS Synthesizer
	KWS KeywordDetector

	ASRName string
	TTSName string
	Speed   float32
}

// NewBundle 组装。返回的 Bundle 拥有这些零件的生命周期（Close 会逐个关）。
func NewBundle(p BundleParts) *Bundle {
	b := &Bundle{
		vad: p.VAD, asr: p.ASR, tts: p.TTS, kws: p.KWS,
		asrName: p.ASRName, ttsName: p.TTSName, speed: p.Speed,
	}
	if b.speed <= 0 {
		b.speed = 1.0
	}
	if ss, ok := b.tts.(SpeedSetter); ok {
		ss.SetSpeed(b.speed)
	}
	return b
}

// 编译期断言：Bundle 仍然是一个 Engine。
var _ Engine = (*Bundle)(nil)

// SpeechProb 走 VAD。
func (b *Bundle) SpeechProb(frame []byte) (float32, error) {
	if b == nil || b.vad == nil {
		return 0, ErrCapability
	}
	return b.vad.SpeechProb(frame)
}

// Transcribe 走 ASR。
func (b *Bundle) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if b == nil || b.asr == nil {
		return "", fmt.Errorf("%w: ASR", ErrCapability)
	}
	return b.asr.Transcribe(ctx, pcm)
}

// Synthesize 走 TTS。
func (b *Bundle) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if b == nil || b.tts == nil {
		return nil, fmt.Errorf("%w: TTS", ErrCapability)
	}
	return b.tts.Synthesize(ctx, text)
}

// SynthesizeWith 实现 VoiceSynthesizer：把"这一句用哪个音色"下发给 TTS 零件。
//
// 为什么要走 Bundle 这一层：路由层（tts_service.go）在"请求的就是引擎里装的
// 那套"时会直接复用引擎实例，只有让 Bundle 也支持带音色合成，这条路才走得通。
// 不支持按请求换音色的零件（本地 VITS）就退回老接口，只把语速落下去。
func (b *Bundle) SynthesizeWith(ctx context.Context, opt SynthOptions) ([]byte, error) {
	if b == nil || b.tts == nil {
		return nil, fmt.Errorf("%w: TTS", ErrCapability)
	}
	if vs, ok := b.tts.(VoiceSynthesizer); ok {
		return vs.SynthesizeWith(ctx, opt)
	}
	if opt.Speed > 0 {
		if ss, ok := b.tts.(SpeedSetter); ok {
			ss.SetSpeed(opt.Speed)
		}
	}
	return b.tts.Synthesize(ctx, opt.Text)
}

// TTSRate 返回合成采样率；没有 TTS 时给一个安全的默认值，
// 免得调用方拿 0 去算播放时长或配置 aplay。
func (b *Bundle) TTSRate() int {
	if b == nil || b.tts == nil {
		return 24000
	}
	if r := b.tts.TTSRate(); r > 0 {
		return r
	}
	return 24000
}

// DetectKeyword 走 KWS；没有 KWS 时永远不命中（退回文本唤醒）。
func (b *Bundle) DetectKeyword(frame []byte) (string, error) {
	if b == nil || b.kws == nil {
		return "", nil
	}
	return b.kws.DetectKeyword(frame)
}

// SetSpeed 调整语速。零件不支持就地调整时记在 Bundle 上，
// 下次换 TTS 时带过去（对云端 TTS 来说语速是请求参数，不是模型状态）。
func (b *Bundle) SetSpeed(speed float32) {
	if speed <= 0 {
		speed = 1
	}
	b.speedMu.Lock()
	b.speed = speed
	b.speedMu.Unlock()
	if ss, ok := b.tts.(SpeedSetter); ok {
		ss.SetSpeed(speed)
	}
}

// Speed 返回当前语速。
func (b *Bundle) Speed() float32 {
	if b == nil {
		return 1
	}
	b.speedMu.RLock()
	defer b.speedMu.RUnlock()
	if b.speed <= 0 {
		return 1
	}
	return b.speed
}

// SetSpeaker 选择音色（零件不支持就忽略）。
func (b *Bundle) SetSpeaker(id int) {
	if b == nil || b.tts == nil {
		return
	}
	if sel, ok := b.tts.(SpeakerSelector); ok {
		sel.SetSpeaker(id)
	}
}

// Status 汇总各零件状态。格式保持老样子 VAD=… ASR=… TTS=… KWS=…，
// 后台和日志都按这个读，别改顺序。
func (b *Bundle) Status() string {
	if b == nil {
		return "空引擎"
	}
	part := func(x interface{ Status() string }) string {
		if x == nil {
			return "未加载"
		}
		return x.Status()
	}
	var vad, asr, tts, kws interface{ Status() string }
	if b.vad != nil {
		vad = b.vad
	}
	if b.asr != nil {
		asr = b.asr
	}
	if b.tts != nil {
		tts = b.tts
	}
	if b.kws != nil {
		kws = b.kws
	}
	names := make([]string, 0, 4)
	names = append(names,
		"VAD="+part(vad),
		"ASR="+part(asr),
		"TTS="+part(tts),
		"KWS="+part(kws),
	)
	return strings.Join(names, " ")
}

// Parts 返回各零件指针，供上层做"按能力分派"（例如只重载 TTS）。
func (b *Bundle) Parts() (vad VoiceDetector, asr Recognizer, tts Synthesizer, kws KeywordDetector) {
	if b == nil {
		return nil, nil, nil, nil
	}
	return b.vad, b.asr, b.tts, b.kws
}

// HasTTS / HasASR / HasKWS / HasVAD 让上层不必手动判空。
func (b *Bundle) HasTTS() bool { return b != nil && b.tts != nil }
func (b *Bundle) HasASR() bool { return b != nil && b.asr != nil }
func (b *Bundle) HasKWS() bool { return b != nil && b.kws != nil }
func (b *Bundle) HasVAD() bool { return b != nil && b.vad != nil }

// Close 逐个释放零件；任何一个报错都继续关其它的，
// 最后把第一个错误返回（C 对象泄漏比错误信息重要得多）。
func (b *Bundle) Close() error {
	if b == nil {
		return nil
	}
	var first error
	b.closeOnce.Do(func() {
		for _, c := range []interface{ Close() error }{b.kws, b.tts, b.asr, b.vad} {
			if c == nil {
				continue
			}
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	})
	return first
}
