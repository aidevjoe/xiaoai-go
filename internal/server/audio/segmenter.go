// Package audio 是服务端的音频处理层。
//
// 设计上把"纯逻辑"与"模型推理"分开：
//   - segmenter / splitter 是纯 Go，可单测、可跨平台跑
//   - engine 里的 ASR/TTS/KWS/VAD 需要 cgo（sherpa-onnx），只在 linux 构建
//
// 这样本机（macOS）也能验证整条流水线的状态与切句行为。
package audio

import "time"

// FrameDuration 是 VAD 的判断粒度。512 采样 @16k = 32ms，与 silero 的窗口一致。
const FrameDuration = 32 * time.Millisecond

// SampleRate 是上行音频采样率。
const SampleRate = 16000

// BytesPerFrame 是一帧的字节数：32ms × 16kHz × 2 字节 = 1024。
const BytesPerFrame = 1024

// Segmenter 把连续的音频帧切成"一句话"。
//
// 规则：
//   - 连续语音达到 MinSpeech 才算开始（过滤咳嗽、鼠标声）
//   - 语音前保留 PreRoll 的音频，避免开头的字被切掉
//   - 连续静音达到 MinSilence 判定说完，把整段交出去
//   - 超过 MaxUtterance 强制切断，防止有人一直说话把内存撑爆
type Segmenter struct {
	MinSpeech    time.Duration
	MinSilence   time.Duration
	PreRoll      time.Duration
	MaxUtterance time.Duration

	preRoll    [][]byte // 环形预缓冲
	preRollIdx int
	preRollLen int
	preRollCap int

	buf          []byte
	inSpeech     bool
	speechCount  int
	silenceCount int

	minSpeechFrames  int
	minSilenceFrames int
	maxBytes         int
}

// NewSegmenter 构造分段器（默认参数按实测调过）。
func NewSegmenter() *Segmenter {
	return NewSegmenterWith(250*time.Millisecond, 300*time.Millisecond, 200*time.Millisecond, 15*time.Second)
}

// NewSegmenterWith 允许自定义阈值。
func NewSegmenterWith(minSpeech, minSilence, preRoll, maxUtterance time.Duration) *Segmenter {
	s := &Segmenter{
		MinSpeech:    minSpeech,
		MinSilence:   minSilence,
		PreRoll:      preRoll,
		MaxUtterance: maxUtterance,
	}
	s.minSpeechFrames = frames(minSpeech)
	s.minSilenceFrames = frames(minSilence)
	s.maxBytes = int(maxUtterance.Seconds() * SampleRate * 2)
	// 容量必须同时容纳"前置静音"和"确认期内的语音帧"，
	// 否则确认语音的那一刻，开头的前置音频已经被覆盖掉了。
	s.preRollCap = frames(preRoll) + s.minSpeechFrames
	if s.preRollCap > 0 {
		s.preRoll = make([][]byte, s.preRollCap)
	}
	return s
}

func frames(d time.Duration) int {
	n := int(d / FrameDuration)
	if n < 1 {
		n = 1
	}
	return n
}

// Push 送入一帧音频及该帧是否为人声。
// 返回非 nil 表示一句话结束，内容为完整 PCM（含少量尾部静音，便于 ASR 收尾）。
func (s *Segmenter) Push(pcm []byte, isSpeech bool) []byte {
	if isSpeech {
		s.speechCount++
		s.silenceCount = 0

		if !s.inSpeech {
			if s.speechCount >= s.minSpeechFrames {
				s.inSpeech = true
				// 把预缓冲里的音频补到开头，避免吞掉第一个字
				s.flushPreRollIntoBuf()
			} else {
				s.pushPreRoll(pcm)
				return nil
			}
		}
		s.buf = append(s.buf, pcm...)
	} else {
		s.silenceCount++
		s.speechCount = 0

		if !s.inSpeech {
			s.pushPreRoll(pcm)
			return nil
		}
		// 说话中遇到静音：先继续收集（可能是句中停顿）
		s.buf = append(s.buf, pcm...)
		if s.silenceCount >= s.minSilenceFrames {
			out := s.buf
			s.reset()
			return out
		}
	}

	// 超长强制切断
	if s.inSpeech && len(s.buf) >= s.maxBytes {
		out := s.buf
		s.reset()
		return out
	}
	return nil
}

func (s *Segmenter) pushPreRoll(pcm []byte) {
	if s.preRollCap == 0 {
		return
	}
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	s.preRoll[s.preRollIdx] = cp
	s.preRollIdx = (s.preRollIdx + 1) % s.preRollCap
	if s.preRollLen < s.preRollCap {
		s.preRollLen++
	}
}

func (s *Segmenter) flushPreRollIntoBuf() {
	if s.preRollCap == 0 || s.preRollLen == 0 {
		return
	}
	start := (s.preRollIdx - s.preRollLen + s.preRollCap) % s.preRollCap
	for i := 0; i < s.preRollLen; i++ {
		f := s.preRoll[(start+i)%s.preRollCap]
		if f != nil {
			s.buf = append(s.buf, f...)
		}
	}
	s.preRollLen = 0
	s.preRollIdx = 0
}

func (s *Segmenter) reset() {
	s.buf = nil
	s.inSpeech = false
	s.speechCount = 0
	s.silenceCount = 0
	s.preRollLen = 0
	s.preRollIdx = 0
}

// Reset 丢弃当前累积（例如被打断）。
func (s *Segmenter) Reset() { s.reset() }

// Speaking 报告是否正在累积一句话。
func (s *Segmenter) Speaking() bool { return s.inSpeech }

// Pending 返回当前已累积的字节数（后台可用来显示"正在听"的长度）。
func (s *Segmenter) Pending() int { return len(s.buf) }
