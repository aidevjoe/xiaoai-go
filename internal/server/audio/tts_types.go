package audio

// TTS 接入层的公共类型。
//
// 目标：接一个新平台 = 新增一个文件（写 provider 工厂 + 声明配置项 + 音色清单），
// 不改这里、不改 registry、不改装配、不改后台页面。
//
// 做到这一点靠三件事：
//   1. Field —— 供应商自己声明"我需要哪些配置"，后台据此渲染表单；
//   2. Voice —— 供应商自己声明"我有哪些音色"，后台据此出下拉框；
//   3. CloneSpec —— 可选能力：供应商自己声明"怎么复刻音色"。
// 上层（后台接口、对话链路）只认这三样，不认识任何一家平台。

import (
	"context"
	"sort"
	"strings"
)

// Field 是一个配置项的声明。
//
// Kind 决定后台渲染什么控件，也决定怎么把字符串解析成值：
//
//	text     单行文本
//	password 密码（后台打码显示，保存后原样回传）
//	number   数字
//	select   枚举（Options 必填）
//	bool     "true" / "false"
//	textarea  多行文本（例如要批量粘贴的音色清单）
type Field struct {
	Key      string        `json:"key"`
	Label    string        `json:"label"`
	Kind     string        `json:"kind"`
	Required bool          `json:"required,omitempty"`
	Default  string        `json:"default,omitempty"`
	Options  []FieldOption `json:"options,omitempty"`
	Help     string        `json:"help,omitempty"`
	// Advanced 的项在后台折叠起来：多数人一辈子不会改（超时、端点覆写）。
	Advanced bool `json:"advanced,omitempty"`
}

// FieldOption 是 select 的一个候选。
type FieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Voice 是一个可选的音色。
//
// ID 是发给平台的原样标识（豆包是 zh_female_vv_uranus_bigtts、Edge 是
// zh-CN-XiaoxiaoNeural）；Name 只用于后台展示。
type Voice struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Lang   string   `json:"lang,omitempty"`
	Gender string   `json:"gender,omitempty"` // female / male / ""
	Tags   []string `json:"tags,omitempty"`
	// Cloned 标记"这是用户自己复刻出来的音色"，后台单独分组，别和官方音色混在一起。
	Cloned bool `json:"cloned,omitempty"`
}

// CloneSpec 是"声音复刻"能力。为 nil 表示这家平台不支持（或本实现没接）。
//
// 三段式对应各家平台几乎一致的流程：
//
//	Upload 提交一段样本音频 → 得到 speaker_id
//	Query  轮询训练状态（训练是异步的，通常几秒到几十秒）
//	List   列出现有复刻音色（填进音色下拉框）
//
// 之所以拆成三个函数而不是一个"克隆并等待"，是因为训练可能几十分钟，
// HTTP 请求扛不住那么久，必须让前端自己轮询。
type CloneSpec struct {
	// Fields 是复刻时额外需要的参数（语种、降噪开关、训练模型等）。
	// 和 provider 的 Fields 分开：这些只在复刻面板里出现。
	Fields []Field
	Upload func(ctx context.Context, cfg map[string]string, req CloneRequest) (CloneState, error)
	Query  func(ctx context.Context, cfg map[string]string, speakerID string) (CloneState, error)
	List   func(ctx context.Context, cfg map[string]string) ([]Voice, error)
	// MinSeconds / MaxSeconds 是样本时长要求，只用于后台提示文案。
	MinSeconds int
	MaxSeconds int
}

// CloneRequest 是一次复刻提交。
type CloneRequest struct {
	SpeakerID string            // 平台侧的音色槽位号（豆包要求先在控制台买/选一个 S_xxx）
	Audio     []byte            // 样本音频原始字节
	Format    string            // wav / mp3 / m4a / pcm…
	Text      string            // 样本对应的文本（可选，给了平台会做一致性校验）
	Params    map[string]string // CloneSpec.Fields 里填的值
}

// 复刻状态取值（统一各家平台的枚举，后台只认这几个）。
const (
	CloneNotFound = "notfound" // 没有这个音色
	CloneTraining = "training" // 训练中
	CloneReady    = "ready"    // 训练完成，可以合成
	CloneFailed   = "failed"   // 训练失败
	CloneActive   = "active"   // 已启用（部分平台训练完成后还要显式启用）
)

// CloneState 是复刻任务的当前状态。
type CloneState struct {
	SpeakerID string `json:"speaker_id"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`    // 失败原因 / 剩余次数之类
	DemoURL   string `json:"demo_url,omitempty"`  // 试听音频地址（部分平台直接给 base64）
	DemoAudio []byte `json:"-"`                   // 试听音频字节（平台返回 base64 时用）
	DemoText  string `json:"demo_text,omitempty"` // 建议的试听文本
}

// SynthOptions 是"这一句话怎么念"的完整描述。
//
// 为什么把音色和语速做成请求参数而不是实例状态：
// 同一台音箱上可能有多个助手（档案），各自音色不同；如果音色是实例状态，
// 用户每换一个助手就得重建一次实例（云端 TTS 重建要重新握手，本地模型
// 重建要几百毫秒 + 几百 MB 内存），而且并发两路对话会互相把音色改掉。
type SynthOptions struct {
	Text  string
	Voice string
	Speed float32
}

// VoiceSynthesizer 是"支持每次请求单独指定音色/语速"的可选能力。
//
// 实现了它，Service 就永远只保留一份实例；没实现（例如老的本地 sherpa 模型，
// 音色由模型文件决定），Service 会退化成"按音色分实例"，仍然能工作。
type VoiceSynthesizer interface {
	SynthesizeWith(ctx context.Context, opt SynthOptions) ([]byte, error)
}

// VoiceChoice 是"用哪个平台、哪个音色、什么参数、多快"。
//
// 它是全局配置与档案覆盖合并之后的产物，对话链路每轮算一次。
// 空字段一律表示"跟随全局"，这样档案里只写一个音色就能工作。
type VoiceChoice struct {
	Provider string            `json:"provider,omitempty"`
	Voice    string            `json:"voice,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Speed    float32           `json:"speed,omitempty"`
}

// IsZero 判断这份选择是否什么都没配（等价于"全跟全局"）。
func (v VoiceChoice) IsZero() bool {
	return strings.TrimSpace(v.Provider) == "" && strings.TrimSpace(v.Voice) == "" &&
		len(v.Params) == 0 && v.Speed <= 0
}

// Clone 复制一份，避免把配置里的 map 传出去被下游改写。
func (v VoiceChoice) Clone() VoiceChoice {
	out := v
	if v.Params != nil {
		out.Params = make(map[string]string, len(v.Params))
		for k, val := range v.Params {
			out.Params[k] = val
		}
	}
	return out
}

// Speaker 是对话层唯一需要的 TTS 能力。
//
// 故意比 Engine 窄：播放器不应该为了念一句话就拿到 ASR/KWS 的句柄。
// Service 与 EngineSpeaker 都实现它，因此"全局一个引擎"和"按档案切供应商"
// 两种实现可以互换，对话层代码不用改。
type Speaker interface {
	// Synthesize 合成一句话，返回 PCM（16bit/mono）与采样率。
	Synthesize(ctx context.Context, text string, v VoiceChoice) ([]byte, int, error)
	// Speed 返回当前全局语速（缓存键与日志用）。
	Speed() float32
	// SetSpeed 设置全局语速（每轮对话的档案语速走 VoiceChoice.Speed，
	// 这里只改"没配语速时的兜底值"）。
	SetSpeed(float32)
	// Available 表示当前是否有可用的合成能力。
	Available() bool
	// Status 返回一句话描述（后台展示）。
	Status() string
}

// engineSpeaker 把老的 Engine 包成 Speaker：忽略 VoiceChoice，音色由模型决定。
//
// 单测与"只有一个本地引擎"的场景用它，省得为了一个接口再造一个假实现。
type engineSpeaker struct{ eng Engine }

// EngineSpeaker 把 Engine 适配成 Speaker。eng 可以为 nil（此时 Available=false）。
func EngineSpeaker(eng Engine) Speaker { return engineSpeaker{eng: eng} }

func (e engineSpeaker) Synthesize(ctx context.Context, text string, _ VoiceChoice) ([]byte, int, error) {
	if e.eng == nil {
		return nil, 0, ErrCapability
	}
	pcm, err := e.eng.Synthesize(ctx, text)
	if err != nil {
		return nil, 0, err
	}
	return pcm, e.eng.TTSRate(), nil
}

func (e engineSpeaker) Speed() float32 {
	if ss, ok := e.eng.(SpeedReader); ok {
		if v := ss.Speed(); v > 0 {
			return v
		}
	}
	return 1
}

func (e engineSpeaker) SetSpeed(v float32) {
	if ss, ok := e.eng.(SpeedSetter); ok {
		ss.SetSpeed(v)
	}
}

func (e engineSpeaker) Available() bool {
	if e.eng == nil {
		return false
	}
	if h, ok := e.eng.(interface{ HasTTS() bool }); ok {
		return h.HasTTS()
	}
	return true
}

func (e engineSpeaker) Status() string {
	if e.eng == nil {
		return "off"
	}
	return e.eng.Status()
}

// ---------- 小工具 ----------

// sortedVoices 去重并按"中文在前、官方在前"排序，让后台下拉框稳定。
func sortedVoices(in []Voice) []Voice {
	seen := make(map[string]bool, len(in))
	out := make([]Voice, 0, len(in))
	for _, v := range in {
		if v.ID == "" || seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := voiceRank(out[i]), voiceRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// voiceRank 决定音色在后台下拉框里的先后：越小越靠前。
//
// 排序目标是"打开下拉框第一眼看到的就是能用的"：
// 中文在前、官方音色在前、带「推荐」标记的最前、1.0 老音色垫底。
func voiceRank(v Voice) int {
	rank := 0
	if v.Cloned {
		rank += 8 // 自己复刻的放后面，别盖住官方清单
	}
	if !strings.HasPrefix(strings.ToLower(v.Lang), "zh") {
		rank += 4
	}
	for _, t := range v.Tags {
		switch strings.ToLower(t) {
		case "推荐":
			rank -= 2
		case "1.0":
			rank += 2
		}
	}
	return rank
}

// cfgStr 取配置值，缺失时返回默认值。
func cfgStr(cfg map[string]string, key, def string) string {
	if v, ok := cfg[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// cfgBool 解析布尔配置，认 true/1/yes/on。
func cfgBool(cfg map[string]string, key string, def bool) bool {
	v, ok := cfg[key]
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return def
}
