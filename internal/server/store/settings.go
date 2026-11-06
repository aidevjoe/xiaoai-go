// Package store 持久化后台可改的全部配置。
//
// 用 JSON 文件而不是数据库：配置项不多、需要人肉可读、方便备份和版本管理。
// 写入用「临时文件 + rename」保证原子性，避免读到半个文件。
//
// 配置分六段，对应后台六个页面：
//
//	server  身份、鉴权、记录保留
//	engine  音频引擎（模型路径、线程、VAD/KWS 阈值）
//	voice   发声（语速、固定应答、切句）
//	dialog  对话策略（分段、超时、轮数）
//	tools   Home Assistant 与工具开关
//	profiles 助手档案（唤醒词 → LLM）
//
// 开发阶段明确不做向后兼容：配置结构随时可能调整，改完直接删掉配置文件
// 让服务端按默认值重建即可（后台的「系统与服务」页可以看到路径）。
// 因此这里没有版本迁移代码，只有 Normalize 负责把离谱的值夹回可用区间。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/tools"
)

// Server 是服务自身的设置。
type Server struct {
	Name        string `json:"name"`         // 后台标题，纯展示
	DeviceToken string `json:"device_token"` // 设备连接鉴权，空=不校验
	KeepTurns   int    `json:"keep_turns"`   // 内存里保留多少轮对话
	SaveAudio   bool   `json:"save_audio"`   // 保存每句话的原始录音（调试用）
	FreeTalk    bool   `json:"free_talk"`    // 免唤醒：任何一句话都当成在跟助手说话（仅调试）
	// WakeByASR 是「用识别文本里的唤醒词来唤醒」，只在没有 KWS 模型时才有意义。
	// 默认关闭：文本唤醒靠编辑距离容错，2 字唤醒词几乎等于"沾一点边就唤醒"，
	// 实测喊「小爱同学」被识别成「小同学」也能命中「小黑」。KWS 可用时不要开它。
	WakeByASR bool   `json:"wake_by_asr"`
	AudioDir  string `json:"audio_dir"` // 录音存放目录，空=配置同级的 audio/
}

// Engine 是音频引擎参数。
//
// 路径留空时按 ModelDir 约定自动推导（见 audio.SherpaConfig），
// 所以大多数情况只需要填 ModelDir。
type Engine struct {
	ModelDir     string  `json:"model_dir"`
	ASRDir       string  `json:"asr_dir"`
	ASRKind      string  `json:"asr_kind"` // sensevoice / paraformer；空=按目录名推断
	TTSModel     string  `json:"tts_model"`
	TTSKind      string  `json:"tts_kind"`      // vits / matcha；空=按是否有声码器推断
	TTSVocoder   string  `json:"tts_vocoder"`   // matcha 专用声码器路径
	Threads      int     `json:"threads"`       // 推理线程；0 = 按部署设备的核数自动
	VADThreshold float32 `json:"vad_threshold"` // 越小越灵敏
	// ASRLang 固定识别语种。留空/auto 时 SenseVoice 会自己猜，
	// 而它在短促、有噪声的片段上经常猜成英文或日文（实测输出 "Okay." "He." "入る."）。
	// 中文场景固定 zh 明显更准。
	ASRLang      string  `json:"asr_lang"`
	KWSEnabled   bool    `json:"kws_enabled"`   // 音箱端本地唤醒词
	KWSScore     float32 `json:"kws_score"`     // 命中加分
	KWSThreshold float32 `json:"kws_threshold"` // 触发阈值

	// ASRProvider / TTSProvider 选"谁来识别/合成"，对应 audio 包的供应商注册表。
	// 留空 = sherpa（本地模型）；填 none = 关闭该能力。
	// 接云端 TTS 时填它的注册名（doubao / edge / http …），本文件一行都不用改。
	ASRProvider string `json:"asr_provider"`
	TTSProvider string `json:"tts_provider"`
	// TTSVoice 是全局音色 ID（豆包 zh_female_vv_uranus_bigtts、Edge zh-CN-XiaoxiaoNeural）。
	// 留空时由供应商自己的默认音色兜底。单个助手可以在档案里覆盖它。
	TTSVoice string `json:"tts_voice"`
	// TTSProviders 是"每个供应商一份参数"：豆包的 app_id/access_key、Edge 的格式…
	//
	// 为什么不是一个扁平的 tts_params：音色是每个助手各取所需，但密钥是账号级的。
	// 如果只有一个 map，用户在档案里把助手切到 Edge 时就拿不到 Edge 的配置，
	// 只能把密钥抄进每个档案 —— 换一次 key 要改十几处。
	TTSProviders map[string]map[string]string `json:"tts_providers,omitempty"`
	// TTSParams 是旧版字段，只用于兼容老配置文件。
	// Normalize 会把它迁移进 TTSProviders[tts_provider] 并清空，之后以
	// TTSProviders 为唯一事实来源（两处并存迟早对不上）。
	TTSParams map[string]string `json:"tts_params,omitempty"`
	ASRParams map[string]string `json:"asr_params,omitempty"`
}

// Voice 是发声相关设置。
type Voice struct {
	Speed            float32 `json:"speed"`              // 语速倍率，1=原速
	WakeReply        string  `json:"wake_reply"`         // 唤醒应答，空=只播提示音
	ExitReply        string  `json:"exit_reply"`         // 退出应答
	ErrorReply       string  `json:"error_reply"`        // 出错应答
	StripEmoji       bool    `json:"strip_emoji"`        // 过滤表情（TTS 念不出来）
	MaxSentenceRunes int     `json:"max_sentence_runes"` // 流式播报的切句长度
	PrebufferMS      int     `json:"prebuffer_ms"`       // 起播前预缓冲
	AckTone          bool    `json:"ack_tone"`           // 说完先"叮"一声，避免等模型时一片安静
	// Backend 决定声音从哪来：
	//   local  —— 服务端本地 VITS（8kHz，清晰度一般，但语速可调、不依赖云端）
	//   device —— 音箱原生 TTS（清晰自然，依赖网络，语速不可调）
	Backend string `json:"backend"`
}

// Tools 是工具集设置。
type Tools struct {
	HAURL   string   `json:"ha_url"`
	HAToken string   `json:"ha_token"`
	Enabled []string `json:"enabled"` // 全局启用的工具名；空=全部启用
	Blocked []string `json:"blocked"` // 永不允许的域名（安全兜底）

	// Search 联网搜索：注入进提示词，而不是让模型调用工具
	Search SearchConfig `json:"search"`
	// Weather 天气来源：ha=读 Home Assistant 的 weather 实体，qweather=和风天气
	Weather WeatherConfig `json:"weather"`
	// Custom 是用户自己配的 HTTP 工具：不用改代码、不用重新编译，
	// 填一个地址就能让模型"多会一件事"。实现见 tools/custom.go。
	Custom []tools.CustomToolSpec `json:"custom,omitempty"`
}

// Dialog 是对话策略。
type Dialog struct {
	// ExitWords 是全局退出词：说这些词就结束连续对话。
	// 档案里自己配了退出词就用档案的，没配用这里的。
	ExitWords      []string `json:"exit_words"`
	SilenceMS      int      `json:"silence_ms"`       // 静音多久判定说完
	MinSpeechMS    int      `json:"min_speech_ms"`    // 最短语音，过滤咳嗽声
	PreRollMS      int      `json:"pre_roll_ms"`      // 语音前保留的音频，防止吞字
	MaxUtteranceMS int      `json:"max_utterance_ms"` // 单段上限，防止一直说
	MaxSentences   int      `json:"max_sentences"`    // 单轮最多播几句
	MaxRounds      int      `json:"max_rounds"`       // 工具调用循环上限
	MaxHistory     int      `json:"max_history"`      // 带入下一轮的历史消息条数
	TimeoutSec     int      `json:"timeout_sec"`      // 静音多久退出连续对话
	// ContextTTLMin 是上下文保鲜期（分钟）：超过这个时长没说话，
	// 下次对话就把历史清空。0=永不过期。
	// 「半小时后再聊昨天那件事」这种场景，把旧对话带进提示词既费 token
	// 又可能让模型答错题（它会把上一轮的话题当成当前语境）。
	ContextTTLMin int `json:"context_ttl_min"`
	PrebufferMS   int `json:"prebuffer_ms"` // 下发前预缓冲（设备侧）
}

// Settings 是全部可持久化配置。
type Settings struct {
	Version  int              `json:"version"`
	Server   Server           `json:"server"`
	Engine   Engine           `json:"engine"`
	Voice    Voice            `json:"voice"`
	Dialog   Dialog           `json:"dialog"`
	Tools    Tools            `json:"tools"`
	Profiles []*agent.Profile `json:"profiles"`
}

// DefaultServer 返回默认的服务设置。
func DefaultServer() Server {
	return Server{
		Name:      "小爱家庭助手",
		KeepTurns: 200,
		WakeByASR: false, // KWS 可用时不需要它；关掉可消除「小爱同学」误唤醒
		AudioDir:  "",
	}
}

// DefaultEngine 返回实测量过的引擎参数。
func DefaultEngine() Engine {
	return Engine{
		ModelDir:     "models",
		Threads:      0,
		VADThreshold: 0.1,
		ASRLang:      "zh",
		KWSEnabled:   true,
		KWSScore:     2.0,
		KWSThreshold: 0.25,
	}
}

// DefaultVoice 返回默认发声设置。
func DefaultVoice() Voice {
	return Voice{
		Speed:            1.0,
		WakeReply:        "我在",
		ExitReply:        "好，有事再叫我",
		ErrorReply:       "抱歉，出了点问题",
		StripEmoji:       true,
		MaxSentenceRunes: 20,
		PrebufferMS:      120,
		AckTone:          true,
		Backend:          "local",
	}
}

// DefaultTools 返回默认工具设置。
func DefaultTools() Tools {
	return Tools{
		Blocked: []string{"lock", "alarm_control_panel", "valve"},
		Search: SearchConfig{
			Provider:   "bocha",
			MaxResults: 3,
			// 4 秒：国内到 Tavily 这类海外服务，TLS 握手 1 秒多、整次检索 2~5 秒，
			// 1.5 秒会在握手阶段就被切掉（实测报 context deadline exceeded）。
			TimeoutMS: 4500,
		},
		Weather: WeatherConfig{
			Enabled:   true,
			Provider:  "ha",
			Days:      0,
			TimeoutMS: 1500,
		},
	}
}

// SearchConfig 是联网搜索（注入式）的配置。
type SearchConfig struct {
	Enabled    bool     `json:"enabled"`
	Provider   string   `json:"provider"` // bocha | tavily | serper
	APIKey     string   `json:"api_key"`
	BaseURL    string   `json:"base_url"` // 走代理或自建网关时填
	MaxResults int      `json:"max_results"`
	TimeoutMS  int      `json:"timeout_ms"`
	Always     bool     `json:"always"`   // 每轮都搜（慢）；false=命中关键词才搜
	Keywords   []string `json:"keywords"` // 自定义触发词，空=内置默认
}

// WeatherConfig 是天气来源配置。
type WeatherConfig struct {
	Enabled   bool   `json:"enabled"`
	Provider  string `json:"provider"` // ha | qweather
	APIKey    string `json:"api_key"`
	Location  string `json:"location"` // 和风 LocationID 或 "经度,纬度"
	Days      int    `json:"days"`     // 预报天数，0=只要实况
	TimeoutMS int    `json:"timeout_ms"`
}

// DefaultDialog 返回实测过的默认参数。
func DefaultDialog() Dialog {
	return Dialog{
		// 静音 300ms 就断句太激进：一句话中间的正常停顿会被切成两段，
		// 后半段变成"碎片"送进 ASR（实测出现 "He." "Oh no." 这种乱码）。
		// open-xiaoai-bridge 用的是 500ms，这里对齐。
		SilenceMS:   500,
		MinSpeechMS: 250,
		// PreRoll 是"判定人声之前先回吐的一小段音频"。
		// 实测（阈值 0.5、房间有背景音）：VAD 往往要 400~600ms 才确认人声，
		// 200ms 的预滚明显不够，表现就是开头一两个字没了
		//（"给我讲一个故事" → "讲一个故事"）。700ms 留足余量，
		// 代价只是每台设备多占 ~25KB 内存。
		PreRollMS:      700,
		MaxUtteranceMS: 15000,
		MaxSentences:   6,
		MaxRounds:      4,
		MaxHistory:     20,
		TimeoutSec:     20,
		ContextTTLMin:  30,
		PrebufferMS:    120,
	}
}

// Defaults 返回一份可用配置（不含档案：档案必须由调用方给出）。
func Defaults() Settings {
	return Settings{
		Version: 1,
		Server:  DefaultServer(),
		Engine:  DefaultEngine(),
		Voice:   DefaultVoice(),
		Dialog:  DefaultDialog(),
		Tools:   DefaultTools(),
	}
}

// Normalize 补齐必要的默认值并做基本校验。
//
// 只处理"零值一定不合理"的字段（线程数、阈值、超时、切句长度），
// 字符串一律尊重原值——留空往往是用户的真实选择（比如唤醒应答留空=只播提示音）。
// 数字也要夹到可用区间，否则一个手滑的 0 会让链路彻底不响应。
func (s *Settings) Normalize() error {
	if s.Version == 0 {
		s.Version = 1
	}

	// ---- server ----
	if s.Server.Name == "" {
		s.Server.Name = DefaultServer().Name
	}
	if s.Server.KeepTurns <= 0 {
		s.Server.KeepTurns = 200
	}
	if s.Server.KeepTurns > 5000 {
		s.Server.KeepTurns = 5000
	}

	// ---- engine ----
	d := DefaultEngine()
	if s.Engine.ModelDir == "" {
		s.Engine.ModelDir = d.ModelDir
	}
	// 0 表示"自动"（按部署设备的核数推导），所以这里不再兜底成固定值
	if s.Engine.Threads < 0 {
		s.Engine.Threads = 0
	}
	if s.Engine.Threads > 16 {
		s.Engine.Threads = 16
	}
	if s.Engine.VADThreshold <= 0 || s.Engine.VADThreshold > 1 {
		s.Engine.VADThreshold = d.VADThreshold
	}
	if s.Engine.KWSScore <= 0 {
		s.Engine.KWSScore = d.KWSScore
	}
	if s.Engine.KWSThreshold <= 0 || s.Engine.KWSThreshold > 1 {
		s.Engine.KWSThreshold = d.KWSThreshold
	}
	// 老配置文件里没有 asr_lang：必须补成 zh，否则 asrLang("") 会退回 auto，
	// 而 auto 正是"识别成英文/日文乱码"的根源。
	if strings.TrimSpace(s.Engine.ASRLang) == "" {
		s.Engine.ASRLang = d.ASRLang
	}
	// 供应商名统一小写去空格；留空表示"用默认的 sherpa"（由装配层决定）
	s.Engine.ASRProvider = strings.ToLower(strings.TrimSpace(s.Engine.ASRProvider))
	s.Engine.TTSProvider = strings.ToLower(strings.TrimSpace(s.Engine.TTSProvider))
	s.Engine.ASRKind = strings.ToLower(strings.TrimSpace(s.Engine.ASRKind))
	s.Engine.TTSKind = strings.ToLower(strings.TrimSpace(s.Engine.TTSKind))
	s.Engine.TTSVoice = strings.TrimSpace(s.Engine.TTSVoice)

	// ---- 多供应商参数 ----
	// 目标形态：tts_providers = { "doubao": {...}, "edge": {...} }，每个平台一份。
	// 老配置只有一个 tts_params，迁移过来后清空，避免"改了一个、另一个还在用旧值"。
	if s.Engine.TTSProviders == nil {
		s.Engine.TTSProviders = map[string]map[string]string{}
	}
	// 重建一份规整过的 map：键小写去空格、值 trim、空值丢掉（空=用默认）。
	// 边遍历边改同一个 map 在 Go 里行为不确定，所以宁可多分配一次。
	norm := make(map[string]map[string]string, len(s.Engine.TTSProviders))
	for name, cfg := range s.Engine.TTSProviders {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || len(cfg) == 0 {
			continue
		}
		clean := make(map[string]string, len(cfg))
		for k, v := range cfg {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "" || v == "" {
				continue
			}
			clean[k] = v
		}
		if len(clean) > 0 {
			norm[key] = clean
		}
	}
	s.Engine.TTSProviders = norm
	if len(s.Engine.TTSParams) > 0 {
		prov := s.Engine.TTSProvider
		if prov == "" {
			prov = "sherpa"
		}
		if _, ok := s.Engine.TTSProviders[prov]; !ok {
			migrated := make(map[string]string, len(s.Engine.TTSParams))
			for k, v := range s.Engine.TTSParams {
				if strings.TrimSpace(v) != "" {
					migrated[k] = strings.TrimSpace(v)
				}
			}
			if len(migrated) > 0 {
				s.Engine.TTSProviders[prov] = migrated
			}
		}
		s.Engine.TTSParams = nil
	}
	// 音色留空时，把参数里的 voice 提上来（老配置把音色写在参数里）
	if s.Engine.TTSVoice == "" {
		if cfg := s.Engine.TTSProviders[s.Engine.TTSProvider]; cfg != nil {
			s.Engine.TTSVoice = strings.TrimSpace(cfg["voice"])
		}
	}

	// ---- voice ----
	dv := DefaultVoice()
	if s.Voice.Speed <= 0 {
		s.Voice.Speed = dv.Speed
	}
	if s.Voice.Speed > 3 {
		s.Voice.Speed = 3
	}
	if s.Voice.MaxSentenceRunes <= 0 {
		s.Voice.MaxSentenceRunes = dv.MaxSentenceRunes
	}
	if s.Voice.MaxSentenceRunes > 80 {
		s.Voice.MaxSentenceRunes = 80
	}
	if s.Voice.PrebufferMS <= 0 {
		s.Voice.PrebufferMS = dv.PrebufferMS
	}
	if s.Voice.Backend != "local" && s.Voice.Backend != "device" {
		s.Voice.Backend = "local"
	}
	// 关掉本地合成却还让服务端逐句合成，等于每句都报"能力未加载: TTS"、整轮无声。
	// 按配置意图纠正为设备原生 TTS（后台选 none 时也会同步改这个字段）。
	if strings.EqualFold(strings.TrimSpace(s.Engine.TTSProvider), "none") && s.Voice.Backend != "device" {
		s.Voice.Backend = "device"
	}

	// ---- dialog ----
	dd := DefaultDialog()
	if len(s.Dialog.ExitWords) == 0 {
		s.Dialog.ExitWords = dd.ExitWords
	}
	if s.Dialog.SilenceMS <= 0 {
		s.Dialog.SilenceMS = dd.SilenceMS
	}
	if s.Dialog.MinSpeechMS <= 0 {
		s.Dialog.MinSpeechMS = dd.MinSpeechMS
	}
	if s.Dialog.PreRollMS <= 0 {
		s.Dialog.PreRollMS = dd.PreRollMS
	}
	if s.Dialog.MaxUtteranceMS <= 0 {
		s.Dialog.MaxUtteranceMS = dd.MaxUtteranceMS
	}
	if s.Dialog.MaxSentences <= 0 {
		s.Dialog.MaxSentences = dd.MaxSentences
	}
	if s.Dialog.MaxRounds <= 0 {
		s.Dialog.MaxRounds = dd.MaxRounds
	}
	if s.Dialog.MaxHistory <= 0 {
		s.Dialog.MaxHistory = dd.MaxHistory
	}
	if s.Dialog.MaxHistory > 60 {
		s.Dialog.MaxHistory = 60
	}
	if s.Dialog.ContextTTLMin < 0 {
		s.Dialog.ContextTTLMin = 0
	}
	if s.Dialog.TimeoutSec <= 0 {
		s.Dialog.TimeoutSec = dd.TimeoutSec
	}
	if s.Dialog.PrebufferMS <= 0 {
		s.Dialog.PrebufferMS = dd.PrebufferMS
	}
	// 前置缓冲不能超过最短语音，否则确认语音时缓冲已被自己覆盖
	// 预滚上限不能按 min_speech 的小倍数卡：VAD 在嘈杂环境里要 400~600ms
	// 才确认人声，按 2 倍卡（min_speech 250 → 上限 500）刚好把开头吃掉。
	// 这里放宽到 1200ms 绝对上限，只防手滑填个离谱的值。
	const maxPreRollMS = 1200
	if s.Dialog.PreRollMS > maxPreRollMS {
		s.Dialog.PreRollMS = maxPreRollMS
	}

	// ---- tools ----
	dt := DefaultTools()
	if len(s.Tools.Blocked) == 0 {
		s.Tools.Blocked = dt.Blocked
	}
	if s.Tools.Search.Provider == "" {
		s.Tools.Search.Provider = dt.Search.Provider
	}
	if s.Tools.Search.MaxResults <= 0 || s.Tools.Search.MaxResults > 10 {
		s.Tools.Search.MaxResults = dt.Search.MaxResults
	}
	// 下限 1s 防手滑，上限 10s 防"一句话等半分钟"。
	if s.Tools.Search.TimeoutMS < 1000 || s.Tools.Search.TimeoutMS > 10000 {
		s.Tools.Search.TimeoutMS = dt.Search.TimeoutMS
	}
	if s.Tools.Weather.Provider == "" {
		s.Tools.Weather.Provider = dt.Weather.Provider
	}
	if s.Tools.Weather.TimeoutMS <= 0 || s.Tools.Weather.TimeoutMS > 10000 {
		s.Tools.Weather.TimeoutMS = dt.Weather.TimeoutMS
	}
	if s.Tools.Weather.Days < 0 || s.Tools.Weather.Days > 7 {
		s.Tools.Weather.Days = 0
	}

	// ---- profiles ----
	if len(s.Profiles) == 0 {
		return fmt.Errorf("store: 至少需要一个助手档案")
	}
	seen := map[string]bool{}
	for i, p := range s.Profiles {
		if p == nil {
			return fmt.Errorf("store: 第 %d 个档案是空的", i+1)
		}
		if p.ID == "" {
			return fmt.Errorf("store: 第 %d 个档案缺少 id", i+1)
		}
		if seen[p.ID] {
			return fmt.Errorf("store: 档案 id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" {
			p.Name = p.ID
		}
		if p.LLM.TimeoutSec <= 0 {
			p.LLM.TimeoutSec = 60
		}
		if p.LLM.MaxTokens <= 0 {
			p.LLM.MaxTokens = 512
		}
		// 唤醒词去空白，避免 "小黑 " 匹配不上
		out := p.WakeWords[:0]
		for _, w := range p.WakeWords {
			if w = strings.TrimSpace(w); w != "" {
				out = append(out, w)
			}
		}
		p.WakeWords = out
		// 音色/供应商留空 = 跟随全局，这里只做去空白
		p.TTSProvider = strings.ToLower(strings.TrimSpace(p.TTSProvider))
		p.TTSSpeaker = strings.TrimSpace(p.TTSSpeaker)
	}
	// 复用 agent 的校验：唤醒词重复、缺 base_url 等都会在这里报出来
	if _, err := agent.NewRegistry(s.Profiles); err != nil {
		return err
	}
	return nil
}

// Manager 负责读写配置文件。
type Manager struct {
	path string

	mu  sync.RWMutex
	cur Settings

	// saveMu 串行化"写盘 + 更新内存"。
	// 之前只有 mu 保护内存，写盘在锁外且 tmp 文件名固定：
	// 两个并发 PUT 会互相覆盖同一个 .tmp，rename 出半个 JSON（配置直接损坏）。
	saveMu sync.Mutex
}

// Open 读取配置；文件不存在时用 def 初始化并落盘。
func Open(path string, def Settings) (*Manager, error) {
	m := &Manager{path: path}

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var s Settings
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("store: 配置文件不是合法 JSON: %w", err)
		}
		if err := s.Normalize(); err != nil {
			return nil, err
		}
		m.cur = s
		return m, nil
	case os.IsNotExist(err):
		if err := def.Normalize(); err != nil {
			return nil, err
		}
		m.cur = def
		if err := m.write(def); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("store: 读取配置失败: %w", err)
	}
}

// Path 返回配置文件路径（后台展示用）。
func (m *Manager) Path() string { return m.path }

// Get 返回当前配置的快照。
//
// 必须是深拷贝：Settings 里有切片、map 和指针（档案），直接返回的话
// 调用方拿着"快照"改一个字段就把内存里的正本改了，而且是跨协程的隐式写。
// 后台页面读配置时顺手规范化一下数组顺序，就会把服务端正在用的配置改掉。
func (m *Manager) Get() Settings {
	m.mu.RLock()
	cur := m.cur
	m.mu.RUnlock()
	return cloneSettings(cur)
}

// cloneSettings 深拷贝配置。手写而不是 JSON 往返：JSON 会把
// map[string]any 里的数字变成 float64，LLM.Extra 里的整数参数会被悄悄改类型。
func cloneSettings(s Settings) Settings {
	out := s
	out.Engine.TTSParams = cloneMap(s.Engine.TTSParams)
	out.Engine.ASRParams = cloneMap(s.Engine.ASRParams)
	if s.Engine.TTSProviders != nil {
		out.Engine.TTSProviders = make(map[string]map[string]string, len(s.Engine.TTSProviders))
		for k, v := range s.Engine.TTSProviders {
			out.Engine.TTSProviders[k] = cloneMap(v)
		}
	}
	out.Tools.Enabled = cloneStrings(s.Tools.Enabled)
	out.Tools.Blocked = cloneStrings(s.Tools.Blocked)
	if s.Tools.Custom != nil {
		out.Tools.Custom = make([]tools.CustomToolSpec, 0, len(s.Tools.Custom))
		for _, c := range s.Tools.Custom {
			cp := c
			cp.Params = cloneMap(c.Params)
			cp.Headers = cloneMap(c.Headers)
			cp.Required = cloneStrings(c.Required)
			out.Tools.Custom = append(out.Tools.Custom, cp)
		}
	}
	out.Dialog.ExitWords = cloneStrings(s.Dialog.ExitWords)
	if s.Profiles != nil {
		out.Profiles = make([]*agent.Profile, 0, len(s.Profiles))
		for _, p := range s.Profiles {
			if p == nil {
				continue
			}
			cp := *p
			// 指针字段必须逐个复制：只做 cp := *p 会让快照与正本共用同一个
			// bool/数值，后台改一次就把服务端正在用的配置也改了。
			if p.NameInPrompt != nil {
				v := *p.NameInPrompt
				cp.NameInPrompt = &v
			}
			cp.WakeWords = cloneStrings(p.WakeWords)
			cp.Tools = cloneStrings(p.Tools)
			cp.ExitWords = cloneStrings(p.ExitWords)
			cp.TTSParams = cloneMap(p.TTSParams)
			if p.Prefetch != nil {
				pf := *p.Prefetch
				cp.Prefetch = &pf
			}
			if p.Voice != nil {
				v := *p.Voice
				if p.Voice.Speed != nil {
					x := *p.Voice.Speed
					v.Speed = &x
				}
				if p.Voice.MaxSentenceRunes != nil {
					x := *p.Voice.MaxSentenceRunes
					v.MaxSentenceRunes = &x
				}
				if p.Voice.MaxSentences != nil {
					x := *p.Voice.MaxSentences
					v.MaxSentences = &x
				}
				if p.Voice.TimeoutSec != nil {
					x := *p.Voice.TimeoutSec
					v.TimeoutSec = &x
				}
				cp.Voice = &v
			}
			out.Profiles = append(out.Profiles, &cp)
		}
	}
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Save 校验后落盘并更新内存。
// 全程持 saveMu：写盘与内存更新必须是一段不可分割的临界区，
// 否则并发保存会丢更新、甚至把配置文件写坏。
func (m *Manager) Save(s Settings) error {
	if err := s.Normalize(); err != nil {
		return err
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if err := m.write(s); err != nil {
		return err
	}
	m.mu.Lock()
	// 存一份深拷贝：调用方手里的那个对象可能稍后还会被改（例如后台先保存
	// 再继续操作同一个 incoming），不能让正本跟着一起变。
	m.cur = cloneSettings(s)
	m.mu.Unlock()
	return nil
}

func (m *Manager) write(s Settings) error {
	if dir := filepath.Dir(m.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 临时文件用唯一名字：即使有漏网的并发写，也不会互相踩到同一个文件
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("store: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是 no-op
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("store: 写配置失败: %w", err)
	}
	if err := tmp.Sync(); err != nil { // 断电时尽量保证不是半个文件
		tmp.Close()
		return fmt.Errorf("store: 落盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: 关闭临时文件失败: %w", err)
	}
	return os.Rename(tmpName, m.path) // 原子替换
}
