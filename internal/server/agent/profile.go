// Package agent 管理"唤醒词 → 助手档案"的路由。
//
// 一个档案 = 一套独立的 LLM 配置 + 系统提示词 + 可选音色。
// 例如：
//
//	"小黑"   → DeepSeek，家庭管家口吻，能用 HA 工具
//	"小智"   → Kimi，闲聊，不给工具
//	"翻译"   → 另一个端点，专门中英互译
//
// 唤醒词由音箱端 KWS 或小爱原生识别给出，匹配到档案后整轮对话都用该档案的配置。
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// LLM 是一个档案用的模型配置。
type LLM struct {
	BaseURL     string         `json:"base_url"`
	APIKey      string         `json:"api_key"`
	Model       string         `json:"model"`
	Temperature float32        `json:"temperature,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	TimeoutSec  int            `json:"timeout_sec,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// Prefetch 控制"哪些信息可以直接预取进提示词"。
// 不接 Home Assistant 的用户可以全关，模型就只靠自身知识回答。
type Prefetch struct {
	Time    bool `json:"time"`    // 当前时间（几乎不用关）
	Weather bool `json:"weather"` // 天气实况（需要 HA 的 weather 实体）
	Devices bool `json:"devices"` // 相关设备及其状态（需要 HA）
}

// VoiceOverride 让单个助手用自己的发声/对话参数。
//
// 为什么用指针：需要区分"没配"和"配成 0"。
// 用值类型的话，「语速 0.8」和「没配（该用全局 1.0）」在 JSON 里都是一个数，
// 没法表达"跟随全局"。指针为 nil 就是跟随全局。
//
// 典型用法：
//
//	「翻译」助手 → 语速 0.9、切句短一点，逐句念
//	「新闻」助手 → 单轮句数放开到 10，允许念长
//	「闲聊」助手 → 走设备原生 TTS（音色更好听），本地合成关掉
type VoiceOverride struct {
	Speed            *float32 `json:"speed,omitempty"`              // 语速倍率
	MaxSentenceRunes *int     `json:"max_sentence_runes,omitempty"` // 切句长度
	MaxSentences     *int     `json:"max_sentences,omitempty"`      // 单轮最多几句
	// Backend 为空表示跟随全局：local=本地合成，device=音箱原生 TTS
	Backend string `json:"backend,omitempty"`
	// TimeoutSec 是这个助手的连续对话静音超时（秒），nil=跟随全局
	TimeoutSec *int `json:"timeout_sec,omitempty"`
}

// Profile 是一个助手档案。
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// NameInPrompt 控制把 Name 写进系统提示词，让助手会自称这个名字。
	// nil 视为开启（老配置没有这个字段时也要生效）；显式 false 可关掉，
	// 方便那些自己在 system_prompt 里写了另一套人格设定的用户。
	NameInPrompt *bool    `json:"name_in_prompt,omitempty"`
	WakeWords    []string `json:"wake_words"`
	SystemPrompt string   `json:"system_prompt"`
	LLM          LLM      `json:"llm"`
	// 这个助手用自己的音色。三项都是"留空跟随全局"：
	//   TTSProvider 换平台（豆包 / edge …），空=跟全局同一个平台
	//   TTSSpeaker  换音色（豆包的 zh_xxx、Edge 的 zh-CN-xxx），空=跟全局
	//   TTSParams   换平台的私有参数（一般不用填，除非这个助手要走另一个账号）
	// 最常见的用法：同一个豆包账号，小美用温柔女声、老张用浑厚男声。
	TTSProvider string            `json:"tts_provider,omitempty"`
	TTSSpeaker  string            `json:"tts_speaker,omitempty"`
	TTSParams   map[string]string `json:"tts_params,omitempty"`
	ExitWords   []string          `json:"exit_words,omitempty"`
	WakeReply   string            `json:"wake_reply,omitempty"` // 唤醒应答，空则用全局「音色→固定应答」
	ExitReply   string            `json:"exit_reply,omitempty"` // 退出应答，空则用全局
	Tools       []string          `json:"tools,omitempty"`      // 允许的工具名；空=全部，["none"]=禁用
	Prefetch    *Prefetch         `json:"prefetch,omitempty"`   // 为空表示全部开启
	// Voice 为空表示发声参数全跟全局；配了就逐项覆盖。
	Voice *VoiceOverride `json:"voice,omitempty"`
}

// IdentityPrompt 生成"我是谁"那一句身份说明。
//
// 只写身份、不覆盖用户自己的语气设定：放在 system_prompt 前面，
// 用户后面写的「回答要口语、简短」仍然生效。
func IdentityPrompt(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return fmt.Sprintf(
		"你的名字是「%s」，用户会这样称呼你。被问到「你是谁」「你叫什么」时，"+
			"用一句简短口语自然地回答「我是%s」；不要编造别的名字，也不要提模型名、厂商或「AI 助手」。",
		name, name)
}

// EffectiveSystemPrompt 返回真正喂给模型的 system：自定义提示词 + 名称身份。
func (p *Profile) EffectiveSystemPrompt() string {
	if p == nil {
		return ""
	}
	own := p.SystemPrompt
	if p.NameInPrompt != nil && !*p.NameInPrompt {
		return own
	}
	identity := IdentityPrompt(p.Name)
	if identity == "" {
		return own
	}
	if strings.TrimSpace(own) == "" {
		return identity
	}
	return identity + "\n\n" + own
}

// AllowsTool 判断该档案是否允许某个工具。
func (p *Profile) AllowsTool(name string) bool {
	if len(p.Tools) == 0 {
		return true
	}
	for _, t := range p.Tools {
		if t == "none" {
			return false
		}
		if t == name {
			return true
		}
	}
	return false
}

// WantPrefetch 判断是否要预取某类信息。
func (p *Profile) WantPrefetch(kind string) bool {
	if p.Prefetch == nil {
		return true // 默认全开
	}
	// 联网检索不在档案级开关里：它由「工具」页的全局配置决定。
	// 放档案级会踩一个坑——老档案里存的 prefetch 没有 web 字段，
	// 反序列化后是 false，等于静默把搜索关掉。
	if kind == "web" {
		return true
	}
	switch kind {
	case "time":
		return p.Prefetch.Time
	case "weather":
		return p.Prefetch.Weather
	case "devices":
		return p.Prefetch.Devices
	}
	return true
}

// Want 实现 tools.PrefetchPolicy，让档案直接决定预取策略。
func (p *Profile) Want(kind string) bool { return p.WantPrefetch(kind) }

// IsExit 判断文本是否是退出词。
func (p *Profile) IsExit(text string) bool {
	words := p.ExitWords
	if len(words) == 0 {
		words = []string{"退出", "退下", "再见", "停止"}
	}
	t := normalize(text)
	for _, w := range words {
		if strings.Contains(t, normalize(w)) {
			return true
		}
	}
	return false
}

// Registry 是档案表，支持热更新。
type Registry struct {
	mu       sync.RWMutex
	profiles []*Profile
	byWord   map[string]*Profile
	fallback *Profile
}

// NewRegistry 构造档案表。第一个档案作为兜底（唤醒词未匹配时使用）。
func NewRegistry(profiles []*Profile) (*Registry, error) {
	r := &Registry{}
	if err := r.Set(profiles); err != nil {
		return nil, err
	}
	return r, nil
}

// Set 替换全部档案（热更新用）。会校验必填字段并重建索引。
func (r *Registry) Set(profiles []*Profile) error {
	if len(profiles) == 0 {
		return fmt.Errorf("agent: 至少需要一个档案")
	}
	byWord := make(map[string]*Profile, len(profiles)*2)
	for i, p := range profiles {
		if p.ID == "" {
			return fmt.Errorf("agent: 第 %d 个档案缺少 id", i+1)
		}
		if p.LLM.BaseURL == "" || p.LLM.Model == "" {
			return fmt.Errorf("agent: 档案 %s 缺少 base_url 或 model", p.ID)
		}
		if len(p.WakeWords) == 0 {
			return fmt.Errorf("agent: 档案 %s 至少需要一个唤醒词", p.ID)
		}
		for _, w := range p.WakeWords {
			key := normalize(w)
			if key == "" {
				continue
			}
			if exist, ok := byWord[key]; ok {
				return fmt.Errorf("agent: 唤醒词 %q 同时属于 %s 和 %s", w, exist.ID, p.ID)
			}
			byWord[key] = p
		}
	}

	r.mu.Lock()
	r.profiles = profiles
	r.byWord = byWord
	r.fallback = profiles[0]
	r.mu.Unlock()
	return nil
}

// All 返回档案快照（按 id 排序，便于后台稳定展示）。
func (r *Registry) All() []*Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Profile, len(r.profiles))
	copy(out, r.profiles)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 按 id 取档案。
func (r *Registry) Get(id string) (*Profile, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.profiles {
		if p.ID == id {
			return p, true
		}
	}
	return nil, false
}

// Fallback 返回兜底档案。
func (r *Registry) Fallback() *Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fallback
}

// Match 按唤醒词找档案。
//
// 匹配策略（从严格到宽松）：
//  1. 归一化后完全相等
//  2. 唤醒词是识别文本的子串（KWS 可能返回"你好小黑"而配置是"小黑"）
//  3. 识别文本包含在唤醒词里（识别丢字）
//
// 都不中则返回兜底档案，保证不会因为一次识别偏差就"叫不应"。
func (r *Registry) Match(keyword string) *Profile {
	k := normalize(keyword)
	if k == "" {
		return r.Fallback()
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if p, ok := r.byWord[k]; ok {
		return p
	}
	for w, p := range r.byWord {
		if strings.Contains(k, w) || strings.Contains(w, k) {
			return p
		}
	}
	return r.fallback
}

// MatchID 按唤醒词返回档案 id（便于日志）。
func (r *Registry) MatchID(keyword string) string {
	if p := r.Match(keyword); p != nil {
		return p.ID
	}
	return ""
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// 去掉空白与常见标点，避免"你好 小黑"匹配不上"你好小黑"
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '。', '，', '！', '？', '、', '.', ',', '!', '?':
			return -1
		}
		return r
	}, s)
}

// LoadFile 从 JSON 文件读档案表。
func LoadFile(path string) ([]*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: 读取档案文件失败: %w", err)
	}
	var wrapper struct {
		Profiles []*Profile `json:"profiles"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return nil, fmt.Errorf("agent: 档案文件不是合法 JSON: %w", err)
	}
	for _, p := range wrapper.Profiles {
		expandEnv(p)
	}
	return wrapper.Profiles, nil
}

// expandEnv 把 "api_key": "${MY_KEY}" 这类引用替换成环境变量的值。
//
// 这样档案文件里可以不含任何密钥：多供应商各自的 key 都放环境变量，
// 配置文件可以放心备份、同步、给别人看。
func expandEnv(p *Profile) {
	lookup := func(s string) string {
		if !strings.Contains(s, "${") {
			return s
		}
		return os.Expand(s, func(k string) string {
			if v, ok := os.LookupEnv(k); ok {
				return v
			}
			return "${" + k + "}" // 没定义就原样保留，便于报错时看出来
		})
	}
	p.LLM.BaseURL = lookup(p.LLM.BaseURL)
	p.LLM.APIKey = lookup(p.LLM.APIKey)
	p.LLM.Model = lookup(p.LLM.Model)
}

// SaveFile 写回档案表（供后台保存配置）。
func SaveFile(path string, profiles []*Profile) error {
	b, err := json.MarshalIndent(struct {
		Profiles []*Profile `json:"profiles"`
	}{profiles}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("agent: 写档案文件失败: %w", err)
	}
	return os.Rename(tmp, path) // 原子替换，避免读到半个文件
}
