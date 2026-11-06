//go:build linux && cgo && sherpa

// 本文件只在「linux + cgo + sherpa 构建标签」下参与编译。
// 原因：sherpa-onnx 是 C++ 库，Go 绑定依赖 cgo 与预编译 .so；
// Mac 开发机上不装这些库，因此用 build tag 隔离，本机跑 mock 引擎。
//
// 构建：go build -tags sherpa ./...（需在 NAS 上，且 LD_LIBRARY_PATH 指向 .so）
//
// 结构说明：这里把「一个引擎管四件事」拆成四个独立零件，
// 每个零件自己一把锁。原先共用一把锁时，Matcha 合成一句话要 4 秒，
// 这 4 秒里 VAD 和 KWS 都拿不到锁——唤醒变迟钝、分段丢帧，
// 而它们跟 TTS 根本没有共享状态。
package audio

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-linux"
)

// ---------- VAD ----------

// sherpaVAD 是 Silero VAD 零件。
type sherpaVAD struct {
	mu  sync.Mutex
	vad *sherpa.VoiceActivityDetector
}

func newSherpaVAD(model string, threshold, minSilence, minSpeech float32) (*sherpaVAD, error) {
	if err := mustExist("VAD 模型", model); err != nil {
		return nil, err
	}
	v := sherpa.NewVoiceActivityDetector(&sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model:              model,
			Threshold:          threshold,
			MinSilenceDuration: minSilence,
			MinSpeechDuration:  minSpeech,
			WindowSize:         512,
			MaxSpeechDuration:  15,
		},
		SampleRate: 16000,
		NumThreads: 1,
		Provider:   "cpu",
	}, 30)
	if v == nil {
		return nil, fmt.Errorf("audio: 加载 VAD 模型失败: %s", model)
	}
	return &sherpaVAD{vad: v}, nil
}

// SpeechProb 判断该帧是否人声。
// VAD 内部会累积语音段，这里每次都把队列排空，只取"当前是否有人说话"
// 这一位信息，分段交给上层的 Segmenter 统一做（否则缓冲无上限增长）。
func (v *sherpaVAD) SpeechProb(frame []byte) (float32, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.vad == nil {
		return 0, fmt.Errorf("%w: VAD", ErrCapability)
	}
	v.vad.AcceptWaveform(pcm16ToFloat(frame))
	for !v.vad.IsEmpty() {
		v.vad.Pop()
	}
	if v.vad.IsSpeech() {
		return 1, nil
	}
	return 0, nil
}

func (v *sherpaVAD) Status() string { return "silero" }

func (v *sherpaVAD) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.vad != nil {
		sherpa.DeleteVoiceActivityDetector(v.vad)
		v.vad = nil
	}
	return nil
}

// ---------- ASR ----------

var svTagRE = regexp.MustCompile(`<\|[^|]*\|>`)

// sherpaASR 是离线识别零件（SenseVoice / Paraformer）。
type sherpaASR struct {
	mu   sync.Mutex
	rec  *sherpa.OfflineRecognizer
	kind string
}

func newSherpaASR(opt ASROptions) (*sherpaASR, error) {
	dir := opt.Dir
	modelPath := opt.Model
	if modelPath == "" {
		modelPath = pickFirst(
			filepath.Join(dir, "model.int8.onnx"),
			filepath.Join(dir, "model.onnx"),
		)
	}
	if modelPath == "" {
		return nil, fmt.Errorf("audio: ASR 目录 %s 里找不到 model.int8.onnx / model.onnx", dir)
	}
	tokens := opt.Tokens
	if tokens == "" {
		tokens = filepath.Join(dir, "tokens.txt")
	}
	if err := mustExist("ASR tokens", tokens); err != nil {
		return nil, err
	}
	kind := asrKind(opt.Kind, dir)
	mc := sherpa.OfflineModelConfig{
		Tokens:     tokens,
		NumThreads: opt.Threads,
		Provider:   "cpu",
		Debug:      opt.Debug,
	}
	// 两种模型的推理图不一样，选错会直接加载失败（或被 C++ abort）。
	// SenseVoice 自带语种与逆文本归一；Paraformer 是纯中文声学模型，没有这两个开关。
	if kind == "paraformer" {
		mc.Paraformer = sherpa.OfflineParaformerModelConfig{Model: modelPath}
	} else {
		mc.SenseVoice = sherpa.OfflineSenseVoiceModelConfig{
			Model:                       modelPath,
			Language:                    asrLang(opt.Lang),
			UseInverseTextNormalization: 1,
		}
	}
	rec := sherpa.NewOfflineRecognizer(&sherpa.OfflineRecognizerConfig{
		FeatConfig:     sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig:    mc,
		DecodingMethod: "greedy_search",
	})
	if rec == nil {
		return nil, fmt.Errorf("audio: 加载 ASR 模型失败: %s", modelPath)
	}
	return &sherpaASR{rec: rec, kind: kind}, nil
}

// Transcribe 识别整句话。
func (a *sherpaASR) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rec == nil {
		return "", fmt.Errorf("%w: ASR", ErrCapability)
	}
	stream := sherpa.NewOfflineStream(a.rec)
	if stream == nil {
		return "", fmt.Errorf("audio: 创建识别流失败")
	}
	defer sherpa.DeleteOfflineStream(stream)

	stream.AcceptWaveform(16000, pcm16ToFloat(pcm))
	a.rec.Decode(stream)
	res := stream.GetResult()
	if res == nil {
		return "", nil
	}
	// SenseVoice 输出带 <|zh|><|NEUTRAL|><|Speech|> 这类标签，必须剥掉
	text := svTagRE.ReplaceAllString(res.Text, "")
	return strings.TrimSpace(text), nil
}

func (a *sherpaASR) Status() string { return a.kind }

func (a *sherpaASR) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rec != nil {
		sherpa.DeleteOfflineRecognizer(a.rec)
		a.rec = nil
	}
	return nil
}

// ---------- TTS ----------

// sherpaTTS 是离线合成零件（VITS / Matcha）。
type sherpaTTS struct {
	mu      sync.Mutex
	tts     *sherpa.OfflineTts
	rate    int
	speed   float32
	speaker int
	kind    string
}

func newSherpaTTS(opt TTSOptions) (*sherpaTTS, error) {
	if err := mustExist("TTS 模型", opt.Model); err != nil {
		return nil, err
	}
	kind := ttsKind(opt.Kind, opt.Vocoder)
	mc := sherpa.OfflineTtsModelConfig{
		NumThreads: opt.Threads,
		Provider:   "cpu",
		Debug:      opt.Debug,
	}
	// VITS 与 Matcha 的推理图不同：Matcha 把声学模型和声码器分开，
	// 采样率也由声码器决定（22.05k），所以必须按类型分别构造。
	if kind == "matcha" {
		mc.Matcha = sherpa.OfflineTtsMatchaModelConfig{
			AcousticModel: opt.Model,
			Vocoder:       opt.Vocoder,
			Tokens:        opt.Tokens,
			Lexicon:       opt.Lexicon,
			DataDir:       opt.DataDir,
			DictDir:       opt.DictDir,
		}
	} else {
		mc.Vits = sherpa.OfflineTtsVitsModelConfig{
			Model:       opt.Model,
			Tokens:      opt.Tokens,
			Lexicon:     opt.Lexicon,
			DataDir:     opt.DataDir,
			DictDir:     opt.DictDir,
			NoiseScale:  0.667,
			NoiseScaleW: 0.8,
			LengthScale: 1.0,
		}
	}
	t := sherpa.NewOfflineTts(&sherpa.OfflineTtsConfig{
		Model:           mc,
		MaxNumSentences: 1,
	})
	if t == nil {
		// sherpa 加载失败只返回 nil，不带原因；把关键路径打出来便于排查
		return nil, fmt.Errorf("audio: TTS 加载失败（model=%s tokens=%s lexicon=%s dictdir=%s）",
			opt.Model, opt.Tokens, opt.Lexicon, opt.DataDir)
	}
	sp := opt.Speed
	if sp <= 0 {
		sp = 1.0
	}
	return &sherpaTTS{tts: t, rate: t.SampleRate(), speed: sp, kind: kind}, nil
}

func (t *sherpaTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tts == nil {
		return nil, fmt.Errorf("%w: TTS", ErrCapability)
	}
	speed := t.speed
	if speed <= 0 {
		speed = 1.0
	}
	a := t.tts.Generate(text, t.speaker, speed)
	if a == nil || len(a.Samples) == 0 {
		return nil, fmt.Errorf("audio: 合成失败: %q", text)
	}
	return floatToPCM16(a.Samples), nil
}

func (t *sherpaTTS) TTSRate() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rate > 0 {
		return t.rate
	}
	return 24000
}

// SetSpeed 只改一个浮点数，不重建模型，后台改完立刻生效。
func (t *sherpaTTS) SetSpeed(speed float32) {
	if speed <= 0 {
		speed = 1.0
	}
	t.mu.Lock()
	t.speed = speed
	t.mu.Unlock()
}

func (t *sherpaTTS) Speed() float32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.speed <= 0 {
		return 1
	}
	return t.speed
}

// SetSpeaker 选择说话人（多说话人 VITS 模型才有意义）。
func (t *sherpaTTS) SetSpeaker(id int) {
	if id < 0 {
		id = 0
	}
	t.mu.Lock()
	t.speaker = id
	t.mu.Unlock()
}

func (t *sherpaTTS) Status() string {
	return fmt.Sprintf("%s(%dHz)", t.kind, t.TTSRate())
}

func (t *sherpaTTS) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tts != nil {
		sherpa.DeleteOfflineTts(t.tts)
		t.tts = nil
	}
	return nil
}

// ---------- KWS ----------

// sherpaKWS 是关键词唤醒零件。
type sherpaKWS struct {
	mu     sync.Mutex
	kws    *sherpa.KeywordSpotter
	stream *sherpa.OnlineStream
}

func newSherpaKWS(opt KWSOptions) (*sherpaKWS, error) {
	k := sherpa.NewKeywordSpotter(&sherpa.KeywordSpotterConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OnlineModelConfig{
			Transducer: sherpa.OnlineTransducerModelConfig{
				Encoder: opt.Encoder,
				Decoder: opt.Decoder,
				Joiner:  opt.Joiner,
			},
			Tokens:     opt.Tokens,
			NumThreads: 1,
			Provider:   "cpu",
			ModelType:  "zipformer2",
		},
		KeywordsFile:      opt.KeywordsFile,
		KeywordsScore:     opt.Score,
		KeywordsThreshold: opt.Threshold,
	})
	if k == nil {
		return nil, fmt.Errorf("audio: 加载 KWS 模型失败: %s", opt.Encoder)
	}
	stream := sherpa.NewKeywordStream(k)
	if stream == nil {
		sherpa.DeleteKeywordSpotter(k)
		return nil, fmt.Errorf("audio: 创建唤醒流失败")
	}
	return &sherpaKWS{kws: k, stream: stream}, nil
}

// DetectKeyword 检测唤醒词，未命中返回空串。
func (k *sherpaKWS) DetectKeyword(frame []byte) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kws == nil || k.stream == nil {
		return "", nil
	}
	k.stream.AcceptWaveform(16000, pcm16ToFloat(frame))
	for k.kws.IsReady(k.stream) {
		k.kws.Decode(k.stream)
		if r := k.kws.GetResult(k.stream); r != nil && r.Keyword != "" {
			kw := r.Keyword
			k.kws.Reset(k.stream)
			return kw, nil
		}
	}
	return "", nil
}

func (k *sherpaKWS) Status() string { return "zipformer2" }

func (k *sherpaKWS) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stream != nil {
		sherpa.DeleteOnlineStream(k.stream)
		k.stream = nil
	}
	if k.kws != nil {
		sherpa.DeleteKeywordSpotter(k.kws)
		k.kws = nil
	}
	return nil
}

// ---------- 供应商注册 ----------
//
// 注册名统一叫 sherpa：本地模型全走这里，靠 Kind 区分架构
// （ASR: sensevoice/paraformer，TTS: vits/matcha）。云端 TTS 之类的
// 新供应商各自注册自己的名字，这里一行都不用改。

func init() {
	RegisterVAD(VADProvider{Name: "silero", Desc: "Silero VAD（随 sherpa 分发）", Local: true,
		New: func(o VADOptions) (VoiceDetector, error) {
			return newSherpaVAD(o.Model, o.Threshold, o.MinSilence, o.MinSpeech)
		}})
	RegisterASR(ASRProvider{Name: "sherpa", Desc: "本地 sherpa-onnx（SenseVoice / Paraformer）", Local: true,
		New: func(o ASROptions) (Recognizer, error) { return newSherpaASR(o) }})
	RegisterTTS(TTSProvider{Name: "sherpa", Desc: "本地 sherpa-onnx（VITS / Matcha）", Local: true,
		New: func(o TTSOptions) (Synthesizer, error) { return newSherpaTTS(o) }})
	RegisterKWS(KWSProvider{Name: "sherpa", Desc: "本地 sherpa-onnx 关键词唤醒", Local: true,
		New: func(o KWSOptions) (KeywordDetector, error) { return newSherpaKWS(o) }})
}

// NewSherpaEngine 按老接口一次性加载全部零件（探测、兼容旧调用点用）。
// 生产路径走各 provider 单独装配（见 cmd/server 的 loadEngine），
// 那样才能只重载 TTS 而不动 ASR。
func NewSherpaEngine(cfg SherpaConfig) (Engine, error) {
	cfg.withDefaults()
	if err := ValidateModelFiles(cfg); err != nil {
		return nil, err
	}
	parts := BundleParts{ASRName: "off", TTSName: "off", Speed: 1.0}

	vad, err := newSherpaVAD(cfg.VADModel, cfg.VADThreshold, cfg.VADMinSilence, cfg.VADMinSpeech)
	if err != nil {
		return nil, err
	}
	parts.VAD = vad

	if cfg.ASRDir != "" {
		asr, err := newSherpaASR(ASROptions{
			Dir: cfg.ASRDir, Kind: cfg.ASRKind, Lang: cfg.ASRLang,
			Threads: cfg.Threads, Debug: cfg.Debug,
		})
		if err != nil {
			_ = vad.Close()
			return nil, err
		}
		parts.ASR, parts.ASRName = asr, asr.kind
	}
	if cfg.TTSModel != "" {
		tts, err := newSherpaTTS(TTSOptions{
			Model: cfg.TTSModel, Kind: cfg.TTSKind, Vocoder: cfg.TTSVocoder,
			Tokens: cfg.TTSTokens, Lexicon: cfg.TTSLexicon,
			DataDir: cfg.TTSDataDir, DictDir: cfg.TTSDictDir,
			Threads: cfg.Threads, Debug: cfg.Debug, Speed: 1.0,
		})
		if err != nil {
			if parts.ASR != nil {
				_ = parts.ASR.Close()
			}
			_ = vad.Close()
			return nil, err
		}
		parts.TTS, parts.TTSName = tts, tts.kind
	}
	kwsReady := fileExists(cfg.KWSEncoder) && fileExists(cfg.KWSDecoder) &&
		fileExists(cfg.KWSJoiner) && fileExists(cfg.KWSTokens) && fileExists(cfg.KeywordsFile)
	if cfg.KWSEnabled && kwsReady {
		kws, err := newSherpaKWS(KWSOptions{
			Encoder: cfg.KWSEncoder, Decoder: cfg.KWSDecoder, Joiner: cfg.KWSJoiner,
			Tokens: cfg.KWSTokens, KeywordsFile: cfg.KeywordsFile,
			Score: cfg.KWSScore, Threshold: cfg.KWSThreshold,
		})
		if err == nil {
			parts.KWS = kws
		}
	}
	return NewBundle(parts), nil
}
