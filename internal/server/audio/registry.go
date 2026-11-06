package audio

// 供应商注册表：把"用哪个 TTS/ASR"变成一个可以配置的字符串。
//
// 之前加一个 TTS 供应商要动 NewSherpaEngine、要动配置结构、要动服务端装配，
// 三处一起改；现在只需要新建一个文件写工厂函数 + init 里注册：
//
//	func init() {
//	    RegisterTTS(TTSProvider{Name: "edge", Desc: "微软 Edge 在线语音", Local: false, New: newEdge})
//	}
//
// 服务端只认 name：配置里写 tts_provider="edge" 就走你这份实现，
// 后台下拉框的候选项也直接从这里列（Providers 返回的就是真装了什么）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// TTSOptions 是传给 TTS 工厂的统一参数。
//
// 本地模型关心 Model/Kind/Vocoder/Threads；云端 API 关心 Params 里的
// voice/rate 之类。统一放在一个结构里，各 provider 取自己用得上的，
// 这样上层装配代码不需要认识任何一家供应商。
type TTSOptions struct {
	Provider string // 注册名，provider 可用来自查
	ModelDir string // 模型根目录
	Model    string // 模型文件
	Kind     string // 架构：vits / matcha / 供应商自定义
	Vocoder  string
	Tokens   string
	Lexicon  string
	DataDir  string
	DictDir  string
	Threads  int
	Speed    float32
	Debug    int
	// Params 是 provider 私有参数（云端 API 的 voice、region 等）。
	Params map[string]string
}

// ASROptions 同理。
type ASROptions struct {
	Provider string
	ModelDir string
	Dir      string // 模型目录（含 model.onnx 与 tokens.txt）
	Model    string
	Tokens   string
	Kind     string // sensevoice / paraformer / 供应商自定义
	Lang     string
	Threads  int
	Debug    int
	Params   map[string]string
}

// VADOptions 是 VAD 工厂参数。目前只有 Silero 一家，但这层抽象让
// "换成 webrtcvad / 云端分段"这种需求不用动上层装配代码。
type VADOptions struct {
	Provider   string
	Model      string
	Threshold  float32
	MinSilence float32 // 秒
	MinSpeech  float32 // 秒
	Threads    int
}

// KWSOptions 是唤醒词检测工厂参数。
type KWSOptions struct {
	Provider     string
	Encoder      string
	Decoder      string
	Joiner       string
	Tokens       string
	KeywordsFile string
	Score        float32
	Threshold    float32
}

// TTSProvider 是一个 TTS 供应商。
//
// Name/Desc/Local/New 是"能跑起来"的最小集合；Fields/Voices/Discover/Clone
// 是"能被后台正确配置"的声明式部分——它们决定了接一家新平台要不要改前端。
//
// 只填 Name/Desc/New 也能工作（后台退化成键值对编辑），这样老 provider
// 不需要一次性补齐声明。
type TTSProvider struct {
	Name  string // 配置里用的名字，小写短横线
	Desc  string // 后台展示
	Local bool   // true=需要本地模型文件；false=走网络
	New   func(TTSOptions) (Synthesizer, error)

	// Fields 声明这个供应商需要哪些配置项，后台据此渲染表单。
	// 键名就是 TTSParams 的键名。
	Fields []Field
	// Voices 是内置音色清单（可以是精选子集），用于后台下拉框。
	// 平台音色太多时不必全列：用户仍可手填音色 ID。
	Voices []Voice
	// Discover 可选：在线拉取完整音色清单（Edge 有开放接口，豆包没有）。
	Discover func(ctx context.Context, cfg map[string]string) ([]Voice, error)
	// Clone 可选：声音复刻能力。
	Clone *CloneSpec
}

// ASRProvider 是一个 ASR 供应商。
type ASRProvider struct {
	Name  string
	Desc  string
	Local bool
	New   func(ASROptions) (Recognizer, error)
}

// VADProvider 是一个 VAD 供应商。
type VADProvider struct {
	Name  string
	Desc  string
	Local bool
	New   func(VADOptions) (VoiceDetector, error)
}

// KWSProvider 是一个唤醒词检测供应商。
type KWSProvider struct {
	Name  string
	Desc  string
	Local bool
	New   func(KWSOptions) (KeywordDetector, error)
}

// ProviderInfo 是对外暴露的候选（后台下拉框用）。
type ProviderInfo struct {
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Local bool   `json:"local"`
}

// TTSProviderInfo 是 TTS 供应商的完整声明，后台靠它把表单画出来。
//
// 刻意内嵌 ProviderInfo：JSON 结构保持扁平的 name/desc/local，
// 老前端读到的字段一个不少，新前端多了 fields/voices/clone 三段。
type TTSProviderInfo struct {
	ProviderInfo
	Fields []Field `json:"fields,omitempty"`
	Voices []Voice `json:"voices,omitempty"`
	// Discover 表示这家平台支持"在线拉取完整音色清单"。
	// 后台据此决定要不要显示「刷新音色」按钮（豆包没有这个接口）。
	Discover bool       `json:"discover,omitempty"`
	Clone    *CloneInfo `json:"clone,omitempty"`
}

// CloneInfo 是对外暴露的"复刻能力"声明（不含函数）。
type CloneInfo struct {
	Fields     []Field `json:"fields,omitempty"`
	MinSeconds int     `json:"min_seconds,omitempty"`
	MaxSeconds int     `json:"max_seconds,omitempty"`
}

var (
	ttsRegMu sync.RWMutex
	ttsReg   = map[string]TTSProvider{}

	asrRegMu sync.RWMutex
	asrReg   = map[string]ASRProvider{}

	vadRegMu sync.RWMutex
	vadReg   = map[string]VADProvider{}

	kwsRegMu sync.RWMutex
	kwsReg   = map[string]KWSProvider{}
)

// RegisterTTS 注册一个 TTS 供应商。重名直接 panic：
// 这是代码结构错误（两个文件抢同一个名字），越早炸越好，不要留到线上。
func RegisterTTS(p TTSProvider) {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if name == "" {
		panic("audio: RegisterTTS 需要 Name")
	}
	if p.New == nil {
		panic("audio: RegisterTTS(" + name + ") 缺少 New")
	}
	ttsRegMu.Lock()
	defer ttsRegMu.Unlock()
	if old, ok := ttsReg[name]; ok {
		panic(fmt.Sprintf("audio: TTS 供应商 %q 重复注册（已有 %q）", name, old.Desc))
	}
	p.Name = name
	ttsReg[name] = p
}

// RegisterASR 注册一个 ASR 供应商。
func RegisterASR(p ASRProvider) {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if name == "" {
		panic("audio: RegisterASR 需要 Name")
	}
	if p.New == nil {
		panic("audio: RegisterASR(" + name + ") 缺少 New")
	}
	asrRegMu.Lock()
	defer asrRegMu.Unlock()
	if old, ok := asrReg[name]; ok {
		panic(fmt.Sprintf("audio: ASR 供应商 %q 重复注册（已有 %q）", name, old.Desc))
	}
	p.Name = name
	asrReg[name] = p
}

// RegisterVAD 注册一个 VAD 供应商。
func RegisterVAD(p VADProvider) {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if name == "" {
		panic("audio: RegisterVAD 需要 Name")
	}
	if p.New == nil {
		panic("audio: RegisterVAD(" + name + ") 缺少 New")
	}
	vadRegMu.Lock()
	defer vadRegMu.Unlock()
	if old, ok := vadReg[name]; ok {
		panic(fmt.Sprintf("audio: VAD 供应商 %q 重复注册（已有 %q）", name, old.Desc))
	}
	p.Name = name
	vadReg[name] = p
}

// RegisterKWS 注册一个唤醒词检测供应商。
func RegisterKWS(p KWSProvider) {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if name == "" {
		panic("audio: RegisterKWS 需要 Name")
	}
	if p.New == nil {
		panic("audio: RegisterKWS(" + name + ") 缺少 New")
	}
	kwsRegMu.Lock()
	defer kwsRegMu.Unlock()
	if old, ok := kwsReg[name]; ok {
		panic(fmt.Sprintf("audio: KWS 供应商 %q 重复注册（已有 %q）", name, old.Desc))
	}
	p.Name = name
	kwsReg[name] = p
}

// TTSProviders 列出全部已注册的 TTS 供应商（按名字排序，后台展示稳定）。
func TTSProviders() []ProviderInfo {
	ttsRegMu.RLock()
	defer ttsRegMu.RUnlock()
	out := make([]ProviderInfo, 0, len(ttsReg))
	for _, p := range ttsReg {
		out = append(out, ProviderInfo{Name: p.Name, Desc: p.Desc, Local: p.Local})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TTSProviderDetails 列出全部 TTS 供应商的完整声明（配置项 + 音色 + 复刻能力）。
// 后台的"在线合成参数"表单就是照着它渲染的。
func TTSProviderDetails() []TTSProviderInfo {
	ttsRegMu.RLock()
	defer ttsRegMu.RUnlock()
	out := make([]TTSProviderInfo, 0, len(ttsReg))
	for _, p := range ttsReg {
		info := TTSProviderInfo{
			ProviderInfo: ProviderInfo{Name: p.Name, Desc: p.Desc, Local: p.Local},
			Fields:       p.Fields,
			Voices:       sortedVoices(p.Voices),
			Discover:     p.Discover != nil,
		}
		if p.Clone != nil {
			info.Clone = &CloneInfo{
				Fields:     p.Clone.Fields,
				MinSeconds: p.Clone.MinSeconds,
				MaxSeconds: p.Clone.MaxSeconds,
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindTTSProvider 取某个供应商的声明（调用方要拿 Fields/Clone 里的函数）。
func FindTTSProvider(name string) (TTSProvider, bool) {
	ttsRegMu.RLock()
	defer ttsRegMu.RUnlock()
	p, ok := ttsReg[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// ASRProviders 列出全部已注册的 ASR 供应商。
func ASRProviders() []ProviderInfo {
	asrRegMu.RLock()
	defer asrRegMu.RUnlock()
	out := make([]ProviderInfo, 0, len(asrReg))
	for _, p := range asrReg {
		out = append(out, ProviderInfo{Name: p.Name, Desc: p.Desc, Local: p.Local})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NewSynthesizer 按名字创建 TTS。名字必须已注册（含内置的 "none"）。
func NewSynthesizer(name string, opt TTSOptions) (Synthesizer, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	ttsRegMu.RLock()
	p, ok := ttsReg[name]
	names := make([]string, 0, len(ttsReg))
	for k := range ttsReg {
		names = append(names, k)
	}
	ttsRegMu.RUnlock()
	if !ok {
		sort.Strings(names)
		return nil, fmt.Errorf("audio: 未知的 TTS 供应商 %q（可用：%s）", name, strings.Join(names, ", "))
	}
	opt.Provider = name
	return p.New(opt)
}

// NewRecognizer 按名字创建 ASR。
func NewRecognizer(name string, opt ASROptions) (Recognizer, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	asrRegMu.RLock()
	p, ok := asrReg[name]
	names := make([]string, 0, len(asrReg))
	for k := range asrReg {
		names = append(names, k)
	}
	asrRegMu.RUnlock()
	if !ok {
		sort.Strings(names)
		return nil, fmt.Errorf("audio: 未知的 ASR 供应商 %q（可用：%s）", name, strings.Join(names, ", "))
	}
	opt.Provider = name
	return p.New(opt)
}

// VADProviders 列出全部已注册的 VAD 供应商。
func VADProviders() []ProviderInfo {
	vadRegMu.RLock()
	defer vadRegMu.RUnlock()
	out := make([]ProviderInfo, 0, len(vadReg))
	for _, p := range vadReg {
		out = append(out, ProviderInfo{Name: p.Name, Desc: p.Desc, Local: p.Local})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KWSProviders 列出全部已注册的唤醒词供应商。
func KWSProviders() []ProviderInfo {
	kwsRegMu.RLock()
	defer kwsRegMu.RUnlock()
	out := make([]ProviderInfo, 0, len(kwsReg))
	for _, p := range kwsReg {
		out = append(out, ProviderInfo{Name: p.Name, Desc: p.Desc, Local: p.Local})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NewVoiceDetector 按名字创建 VAD。
func NewVoiceDetector(name string, opt VADOptions) (VoiceDetector, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	vadRegMu.RLock()
	p, ok := vadReg[name]
	vadRegMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("audio: 未知的 VAD 供应商 %q（可用：%s）", name, names(vadRegKeys()))
	}
	opt.Provider = name
	return p.New(opt)
}

// NewKeywordDetector 按名字创建 KWS。
func NewKeywordDetector(name string, opt KWSOptions) (KeywordDetector, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	kwsRegMu.RLock()
	p, ok := kwsReg[name]
	kwsRegMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("audio: 未知的 KWS 供应商 %q（可用：%s）", name, names(kwsRegKeys()))
	}
	opt.Provider = name
	return p.New(opt)
}

func vadRegKeys() []string {
	vadRegMu.RLock()
	defer vadRegMu.RUnlock()
	out := make([]string, 0, len(vadReg))
	for k := range vadReg {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func kwsRegKeys() []string {
	kwsRegMu.RLock()
	defer kwsRegMu.RUnlock()
	out := make([]string, 0, len(kwsReg))
	for k := range kwsReg {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func names(ss []string) string { return strings.Join(ss, ", ") }

// HasTTSProvider 判断某个名字是否存在（配置校验用，比字符串列表快）。
func HasTTSProvider(name string) bool {
	ttsRegMu.RLock()
	defer ttsRegMu.RUnlock()
	_, ok := ttsReg[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// HasASRProvider 判断某个 ASR 名字是否存在。
func HasASRProvider(name string) bool {
	asrRegMu.RLock()
	defer asrRegMu.RUnlock()
	_, ok := asrReg[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// TTSProviderLocal 判断该 TTS 供应商是否需要本地模型文件。
// 装配层用它决定"没有模型路径时还该不该尝试加载"：
// 云端供应商不需要 TTSModel，本地供应商没有它就没法工作。
func TTSProviderLocal(name string) bool {
	ttsRegMu.RLock()
	defer ttsRegMu.RUnlock()
	p, ok := ttsReg[strings.ToLower(strings.TrimSpace(name))]
	return ok && p.Local
}

// ASRProviderLocal 同理。
func ASRProviderLocal(name string) bool {
	asrRegMu.RLock()
	defer asrRegMu.RUnlock()
	p, ok := asrReg[strings.ToLower(strings.TrimSpace(name))]
	return ok && p.Local
}

// nilSynthesizer 是"故意不提供 TTS"的实现：配置里选 none 时用它，
// 让上层不用到处判空（状态里会明确写出 off，不会伪装成坏掉）。
type nilSynthesizer struct{}

func (nilSynthesizer) Synthesize(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("%w: TTS 已关闭", ErrCapability)
}
func (nilSynthesizer) TTSRate() int   { return 24000 }
func (nilSynthesizer) Status() string { return "off" }
func (nilSynthesizer) Close() error   { return nil }

// nilRecognizer 同理。
type nilRecognizer struct{}

func (nilRecognizer) Transcribe(context.Context, []byte) (string, error) {
	return "", fmt.Errorf("%w: ASR 已关闭", ErrCapability)
}
func (nilRecognizer) Status() string { return "off" }
func (nilRecognizer) Close() error   { return nil }

func init() {
	// 内置的"关闭"选项：想只用设备原生 TTS 的人可以显式选它，
	// 而不是填一个不存在的名字然后看报错。
	RegisterTTS(TTSProvider{Name: "none", Desc: "关闭本地合成，改由音箱发声", Local: true,
		New: func(TTSOptions) (Synthesizer, error) { return nilSynthesizer{}, nil }})
	RegisterASR(ASRProvider{Name: "none", Desc: "关闭识别", Local: true,
		New: func(ASROptions) (Recognizer, error) { return nilRecognizer{}, nil }})
}
