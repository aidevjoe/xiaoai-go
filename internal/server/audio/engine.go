package audio

import (
	"context"
	"errors"
	"sync"
)

// Engine 是需要模型推理的能力集合。真实实现基于 sherpa-onnx（cgo），
// 测试与 Mac 开发用 MockEngine。
type Engine interface {
	// SpeechProb 返回该帧是人声的概率（0-1）。VAD。
	SpeechProb(frame []byte) (float32, error)
	// Transcribe 把一段完整语音转成文字。ASR。
	Transcribe(ctx context.Context, pcm []byte) (string, error)
	// Synthesize 把文本合成 16bit/mono PCM。采样率由 TTSRate 给出。
	Synthesize(ctx context.Context, text string) ([]byte, error)
	// TTSRate 返回合成音频的采样率（VITS 模型常见 16000 或 22050）。
	TTSRate() int
	// DetectKeyword 检测唤醒词，未命中返回空串。KWS（可选能力）。
	DetectKeyword(frame []byte) (string, error)
	// Status 返回一句话描述引擎状态（后台与日志用）。
	Status() string
	// Close 释放模型。
	Close() error
}

// ErrNotImplemented 表示某个能力尚未接入。
var ErrNotImplemented = errors.New("audio: 该能力尚未实现")

// SpeedSetter 见 components.go（随能力拆分一起定义）。

// Pipeline 把"音频帧 → 人声概率 → 分段 → 识别"串起来。
//
// 它不含任何模型代码，只做状态与调度，因此可在任意平台单测。
type Pipeline struct {
	eng    Engine
	seg    *Segmenter
	onText func(text string, pcm []byte)

	mu       sync.Mutex
	speaking bool
}

// NewPipeline 构造流水线。onText 在识别出完整一句话后回调。
func NewPipeline(eng Engine, onText func(text string, pcm []byte)) *Pipeline {
	return &Pipeline{eng: eng, seg: NewSegmenter(), onText: onText}
}

// NewPipelineWith 用自定义分段参数构造流水线（后台改了阈值就不用重启）。
// seg 为 nil 时退回默认参数。
func NewPipelineWith(eng Engine, seg *Segmenter, onText func(text string, pcm []byte)) *Pipeline {
	if seg == nil {
		seg = NewSegmenter()
	}
	return &Pipeline{eng: eng, seg: seg, onText: onText}
}

// Push 送入一帧 PCM（16k/S16/mono，1024 字节）。
// 返回识别结果（若有）。
func (p *Pipeline) Push(ctx context.Context, pcm []byte) (string, error) {
	prob, err := p.eng.SpeechProb(pcm)
	if err != nil {
		return "", err
	}
	isSpeech := prob >= 0.5

	utterance := p.seg.Push(pcm, isSpeech)
	if utterance == nil {
		p.mu.Lock()
		p.speaking = p.seg.Speaking()
		p.mu.Unlock()
		return "", nil
	}

	text, err := p.eng.Transcribe(ctx, utterance)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.speaking = false
	p.mu.Unlock()
	if p.onText != nil && text != "" {
		p.onText(text, utterance)
	}
	return text, nil
}

// Speaking 报告当前是否正在累积一句话。
func (p *Pipeline) Speaking() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.speaking
}

// Reset 丢弃当前累积（打断时用）。
func (p *Pipeline) Reset() {
	p.seg.Reset()
	p.mu.Lock()
	p.speaking = false
	p.mu.Unlock()
}

// MockEngine 用于单测与无模型环境。
//
// 注意：并发测试会同时调用它，所以内部状态要加锁保护。
type MockEngine struct {
	mu sync.Mutex

	Prob      float32                         // 固定返回的人声概率
	Text      string                          // 固定返回的识别结果
	Audio     []byte                          // 固定返回的合成结果
	CallCount int                             // Transcribe 被调用次数
	TTSInput  []string                        // 记录合成过的文本
	Hook      func(pcm []byte) (string, bool) // 可选：按音频内容动态决定识别结果
	ProbFunc  func([]byte) float32            // 可选：按音频内容动态给出人声概率
}

// NewMockEngine 构造 mock。
func NewMockEngine(prob float32, text string) *MockEngine {
	return &MockEngine{Prob: prob, Text: text, Audio: make([]byte, 480)}
}

func (m *MockEngine) SpeechProb(frame []byte) (float32, error) {
	if m.ProbFunc != nil {
		return m.ProbFunc(frame), nil
	}
	return m.Prob, nil
}

func (m *MockEngine) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	m.mu.Lock()
	m.CallCount++
	m.mu.Unlock()
	if m.Hook != nil {
		if s, ok := m.Hook(pcm); ok {
			return s, nil
		}
	}
	return m.Text, nil
}

func (m *MockEngine) Synthesize(ctx context.Context, text string) ([]byte, error) {
	m.mu.Lock()
	m.TTSInput = append(m.TTSInput, text)
	m.mu.Unlock()
	return m.Audio, nil
}

// Calls 返回 Transcribe 被调用的次数（并发安全）。
func (m *MockEngine) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.CallCount
}

// TTSInputs 返回合成过的文本快照（并发安全）。
func (m *MockEngine) TTSInputs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.TTSInput))
	copy(out, m.TTSInput)
	return out
}

func (m *MockEngine) TTSRate() int { return 24000 }

func (m *MockEngine) Status() string { return "mock 引擎（无真实模型）" }

func (m *MockEngine) DetectKeyword(frame []byte) (string, error) { return "", nil }

func (m *MockEngine) Close() error { return nil }
