package dialog

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
	"xiaoai-go/internal/server/llm"
	"xiaoai-go/internal/server/tools"
)

// Turn 记录一轮对话的可观测数据（后台瀑布图用）。
type Turn struct {
	Device       string `json:"device,omitempty"`
	ProfileID    string `json:"profile_id"`
	WakeWord     string `json:"wake_word,omitempty"`
	Text         string `json:"text"`
	Reply        string `json:"reply"`
	Rounds       int    `json:"rounds"`      // 模型调用次数
	ToolRounds   int    `json:"tool_rounds"` // 真正执行工具的轮数
	Sentences    int    `json:"sentences"`
	FirstDeltaMS int64  `json:"first_delta_ms"`
	// FirstAudioMS 是"第一段音频下发出去"的时刻——用户真正听到声音看的是它，
	// 而不是模型首个 token。两者之差 = 首句合成 + 下发。
	FirstAudioMS int64   `json:"first_audio_ms"`
	RoundMS      []int64 `json:"round_ms,omitempty"`
	TotalMS      int64   `json:"total_ms"`
	Err          string  `json:"err,omitempty"`
	// Pending 表示"已经听到了，正在处理"，此时 Reply 还是空的。
	// 后台据此立刻显示这一句，不用等模型回答完。
	Pending bool      `json:"pending,omitempty"`
	At      time.Time `json:"at"`
}

// Replies 是几句固定应答（后台可改）。留空则不播。
type Replies struct {
	Wake  string `json:"wake"`
	Exit  string `json:"exit"`
	Error string `json:"error"`
}

// DefaultReplies 返回默认应答语。
func DefaultReplies() Replies {
	return Replies{Wake: "在呢", Exit: "那我先退下了", Error: "抱歉，出了点问题"}
}

// Manager 为每台设备维护会话，并复用各档案的 LLM 客户端。
type Manager struct {
	Reg   *agent.Registry
	Tools *tools.Registry
	// Speech 是"把一句话念出来"的唯一入口。
	// 生产环境注入 audio.Service（按档案切供应商），
	// 测试与"只有一个本地模型"的场景注入 audio.EngineSpeaker(eng)。
	Speech audio.Speaker
	// speechExplicit 标记 Speech 是否被外部显式换成过（Service）。
	// 是的话 SetEngine 不能覆盖它——否则装上新引擎时会把路由层顶掉，
	// 表现就是"重载引擎后所有助手又变回同一个声音"。
	speechExplicit bool

	Timeout time.Duration // 静音多久回到 idle
	OnTurn  func(*Turn)   // 回调：写日志 / 推后台

	// 后台可调参数（applySettings 热更新，改完不用重启）
	Speak   SpeakOptions // 切句长度、表情过滤、单轮句数上限
	Replies Replies      // 固定应答
	// Fixed 缓存固定应答的合成结果（唤醒/退出/出错），
	// 让"我在""诶"这类每次都要说的短句不必反复合成。
	Fixed *ReplyCache
	// ExitWords 是全局退出词（档案没配时用）
	ExitWords []string
	// GlobalSpeed 是全局语速。档案可以覆盖它，所以必须记住"原始值"，
	// 否则换回没配语速的档案时恢复不了。
	GlobalSpeed float32
	// GlobalVoice 是全局音色选择（供应商/音色/参数）。
	// 档案只覆盖它显式配置的那几项，其余继续跟全局。
	GlobalVoice audio.VoiceChoice

	// ContextTTL 是上下文保鲜期：超过这个时长没说话，下一轮就丢掉历史。
	// 0 表示永不过期。
	ContextTTL time.Duration
	MaxRounds  int // 工具调用循环上限
	MaxHistory int // 保留多少条历史消息

	// BaseCtx 是会话的根上下文。必须是长生命周期（进程级），
	// 不能用某次 HTTP 请求的 ctx——请求一结束上下文就被取消，
	// 后续的模型请求会全部失败（"context canceled"）。
	BaseCtx context.Context

	// Context 可选：每轮预取上下文（当前时间、天气、相关设备），
	// 直接拼进 system 提示词，避免为这些信息多跑一轮工具调用。
	Context func(ctx context.Context, p *agent.Profile, text string) string

	log *log.Logger

	mu       sync.Mutex
	sessions map[string]*Session
	runners  map[string]*llm.Runner // profileID → runner
}

// NewManager 构造管理器。
func NewManager(reg *agent.Registry, tl *tools.Registry, eng audio.Engine, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{
		BaseCtx: context.Background(),
		Reg:     reg, Tools: tl,
		Speech:     audio.EngineSpeaker(eng),
		Timeout:    20 * time.Second,
		Speak:      DefaultSpeakOptions(),
		Replies:    DefaultReplies(),
		MaxRounds:  4,
		MaxHistory: 20,
		log:        logger,
		sessions:   make(map[string]*Session),
		runners:    make(map[string]*llm.Runner),
	}
}

// SetRegistry 热替换档案表并清空缓存的 runner（配置改动后调用）。
func (m *Manager) SetRegistry(reg *agent.Registry) {
	m.mu.Lock()
	m.Reg = reg
	m.runners = make(map[string]*llm.Runner)
	m.mu.Unlock()
}

// SetTools 热替换工具注册表并清空缓存的 runner。
//
// 必须清 runner：runner 在构造时就把 filteredTools{reg: m.Tools} 抓进去了，
// 不清的话"后台改工具开关"要重启进程才生效（之前就是这个行为）。
func (m *Manager) SetTools(t *tools.Registry) {
	m.mu.Lock()
	m.Tools = t
	m.runners = make(map[string]*llm.Runner)
	m.mu.Unlock()
}

// historyLimit 返回保留的历史消息条数。
func (m *Manager) historyLimit() int {
	if m.MaxHistory <= 0 {
		return 20
	}
	return m.MaxHistory
}

// SetEngine 热替换音频引擎（引擎重载后用）。
//
// 只在 Speech 还是"直接用引擎"的适配器时才更新它。一旦外面注入了
// audio.Service（多供应商路由），引擎归属由 Service 自己管，
// 这里覆盖就等于把路由层踢掉——那会导致所有档案的音色忽然失效。
func (m *Manager) SetEngine(eng audio.Engine) {
	m.mu.Lock()
	speechExplicit := m.speechExplicit
	m.mu.Unlock()
	if speechExplicit {
		return
	}
	m.mu.Lock()
	m.Speech = audio.EngineSpeaker(eng)
	m.mu.Unlock()
}

// SetSpeech 注入自定义合成入口（生产环境是 audio.Service）。
func (m *Manager) SetSpeech(sp audio.Speaker) {
	m.mu.Lock()
	m.Speech = sp
	m.speechExplicit = true
	m.mu.Unlock()
}

// ApplySpeed 把语速落到当前合成入口上（不支持就地调速时静默忽略）。
//
// 语速是"兜底值"：每轮对话真正生效的是 speakOptionsOf 算出来的
// VoiceChoice.Speed（云端 TTS 把它当请求参数，本地模型才需要改状态）。
func (m *Manager) ApplySpeed(speed float32) {
	if speed <= 0 {
		return
	}
	m.mu.Lock()
	sp := m.Speech
	m.mu.Unlock()
	if sp == nil {
		return
	}
	sp.SetSpeed(speed)
}

// speaker 取当前合成入口（并发安全）。
func (m *Manager) speaker() audio.Speaker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Speech
}

// VoiceOf 返回该档案实际使用的音色选择（全局 → 档案覆盖）。
//
// 只覆盖档案显式写了的项：档案里只填了音色、没填供应商，就应该
// 继续用全局供应商——这是最常见的用法（同一个豆包账号，不同助手不同音色）。
func (m *Manager) VoiceOf(p *agent.Profile) audio.VoiceChoice {
	v := m.GlobalVoice.Clone()
	if p == nil {
		return v
	}
	if s := strings.TrimSpace(p.TTSProvider); s != "" {
		// 换平台时必须丢掉原平台的参数：global 的 api_key 是豆包的，
		// 带给 Edge 既不生效，也等于把密钥发给了不相干的服务。
		if !strings.EqualFold(s, v.Provider) && len(p.TTSParams) == 0 {
			v.Params = nil
		}
		v.Provider = s
	}
	if s := strings.TrimSpace(p.TTSSpeaker); s != "" {
		v.Voice = s
	}
	if len(p.TTSParams) > 0 {
		v.Params = p.TTSParams
	}
	v.Speed = m.SpeedOf(p)
	return v
}

// speakOptionsOf 把"全局播报参数"与"档案覆盖"合并成这一轮的实际参数。
func (m *Manager) speakOptionsOf(p *agent.Profile) SpeakOptions {
	opt := m.Speak
	opt.Voice = m.VoiceOf(p)
	if p == nil || p.Voice == nil {
		return opt
	}
	v := p.Voice
	if v.MaxSentenceRunes != nil && *v.MaxSentenceRunes > 0 {
		opt.MaxSentenceRunes = *v.MaxSentenceRunes
	}
	if v.MaxSentences != nil {
		// 档案显式写 0 表示"不限句数"，这里必须尊重，不能被全局的 6 覆盖
		opt.MaxSentences = *v.MaxSentences
	}
	if v.Backend != "" {
		opt.DeviceTTS = v.Backend == "device"
	}
	return opt
}

// SpeedOf 返回该档案实际该用的语速（没配就用全局）。
func (m *Manager) SpeedOf(p *agent.Profile) float32 {
	if p != nil && p.Voice != nil && p.Voice.Speed != nil && *p.Voice.Speed > 0 {
		return *p.Voice.Speed
	}
	if m.GlobalSpeed > 0 {
		return m.GlobalSpeed
	}
	return 1
}

// TimeoutOf 返回该档案的连续对话静音超时（没配就用全局）。
func (m *Manager) TimeoutOf(p *agent.Profile) time.Duration {
	if p != nil && p.Voice != nil && p.Voice.TimeoutSec != nil && *p.Voice.TimeoutSec > 0 {
		return time.Duration(*p.Voice.TimeoutSec) * time.Second
	}
	return m.Timeout
}

// ReportEvent 把"没走模型"的交互（唤醒、退出）也记进对话记录。
//
// 不记的话，用户在"最近对话"里只看到问答，看不到"你好小黑""退下"，
// 会以为这些操作没生效。
func (m *Manager) ReportEvent(t *Turn) {
	if m.OnTurn == nil || t == nil {
		return
	}
	if t.At.IsZero() {
		t.At = now()
	}
	m.OnTurn(t)
}

// Session 取（或创建）某台设备的会话。
func (m *Manager) Session(id string, tgt Target) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.tgt = tgt
		return s
	}
	s := &Session{id: id, tgt: tgt, mgr: m, state: StateIdle}
	m.sessions[id] = s
	return s
}

// runnerFor 按档案构造（并缓存）Runner。
// 不同档案可以指向完全不同的模型端点与提示词——这正是"多唤醒词多 LLM"的落点。
func (m *Manager) runnerFor(p *agent.Profile) *llm.Runner {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runners[p.ID]; ok {
		return r
	}
	cfg := llm.Config{
		BaseURL:     p.LLM.BaseURL,
		APIKey:      p.LLM.APIKey,
		Model:       p.LLM.Model,
		Temperature: p.LLM.Temperature,
		MaxTokens:   p.LLM.MaxTokens,
		Extra:       p.LLM.Extra,
	}
	if p.LLM.TimeoutSec > 0 {
		cfg.Timeout = time.Duration(p.LLM.TimeoutSec) * time.Second
	}
	r := &llm.Runner{
		Client:    llm.New(cfg),
		System:    p.EffectiveSystemPrompt(),
		MaxRounds: m.MaxRounds,
	}
	if r.MaxRounds <= 0 {
		r.MaxRounds = 4
	}
	if m.Tools != nil {
		r.Exec = &filteredTools{reg: m.Tools, p: p}
	}
	m.runners[p.ID] = r
	return r
}

// Lookup 按设备 id 取已存在的会话（不存在返回 false）。
func (m *Manager) Lookup(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Sweep 定期回收超时会话。设备忘关或用户走开时不能让状态一直挂着。
func (m *Manager) Sweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// 先在 m.mu 下拷一份会话指针，再放开 m.mu 去读每个会话。
			// 不能"拿着 m.mu 等 s.mu"：任何持有 s.mu 又要 m.mu 的路径
			// 都会和它形成锁序反转（这次事故里就是 Sweep 被永久卡住）。
			m.mu.Lock()
			sessions := make([]*Session, 0, len(m.sessions))
			for _, s := range m.sessions {
				sessions = append(sessions, s)
			}
			m.mu.Unlock()

			var expired []*Session
			for _, s := range sessions {
				// 超时按档案取：不同助手可以配不同的"多久算走开"
				if s.Expired(s.idleTimeout()) {
					expired = append(expired, s)
				}
			}
			for _, s := range expired {
				st := s.State()
				if st == StateIdle {
					continue
				}
				// 正在跑模型 / 正在播报时不能按"静音超时"处理。
				//
				// 这里的超时本意是"用户半天没再说话就退出对话"，
				// 但对在途请求调用 Idle() 会 cancel 掉 run ctx，
				// 于是 HTTP 请求被中断，日志里就是那句
				//   llm: 请求失败: Post "…/chat/completions": context canceled
				// 思考型模型（kimi-k3.1-pro 等）首字动辄 20 秒以上，
				// 正好撞上 20 秒的对话超时——用户看到的就是"问了没反应"。
				if st == StateThinking || st == StateSpeaking {
					// 兜底：真卡死了也不能永远占着，给一个宽松上限
					if s.Expired(3 * m.Timeout) {
						s.mgr.log.Printf("[%s] 处理超时（%s），强制回到空闲", s.id, st)
						s.Idle()
					}
					continue
				}
				s.mgr.log.Printf("[%s] 静音超时，回到空闲", s.id)
				s.Idle()
			}
		}
	}
}

// InvalidateProfile 在档案被修改后丢掉缓存的 runner（热更新用）。
func (m *Manager) InvalidateProfile(id string) {
	m.mu.Lock()
	delete(m.runners, id)
	m.mu.Unlock()
}

// filteredTools 按档案的工具白名单过滤。
type filteredTools struct {
	reg *tools.Registry
	p   *agent.Profile
}

func (f *filteredTools) Specs() []llm.Tool {
	return f.reg.Specs(func(name string) bool { return f.p.AllowsTool(name) })
}

func (f *filteredTools) Exec(ctx context.Context, name, args string) (string, error) {
	if !f.p.AllowsTool(name) {
		return `{"ok":false,"error":"该助手未启用此工具"}`, nil
	}
	return f.reg.Exec(ctx, name, args)
}

// Session 是一台设备的对话状态。
type Session struct {
	id  string
	tgt Target
	mgr *Manager

	mu         sync.Mutex
	profile    *agent.Profile
	history    []llm.Message
	state      string
	lastActive time.Time
	ctx        context.Context
	cancel     context.CancelFunc

	// spoken 记住最近说过的话（唤醒应答、模型回答、退出语），见 isEcho。
	spoken []spokenItem
}

// spokenItem 是"这台音箱最近念过的一句"。
type spokenItem struct {
	text string
	at   time.Time
}

// echoWindow 只在这段时间内做回声比对：再早的话不可能是自己说漏的。
const echoWindow = 20 * time.Second

// noteSpoken 记录一句刚播出去的文本（最近的放最前面，最多留 4 句）。
func (s *Session) noteSpoken(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	s.spoken = append([]spokenItem{{text: text, at: now()}}, s.spoken...)
	if len(s.spoken) > 4 {
		s.spoken = s.spoken[:4]
	}
	s.mu.Unlock()
}

// isEcho 判断这段识别文本是不是"音箱听到了自己的尾巴"。
//
// 播报结束后闭麦只再留几百毫秒，房间混响和迟到的音频尾巴仍可能被收进来，
// 识别出来的往往是刚说完那几句的最后几个字（实测："尽管说""您说""再说一遍吧"
// 被当成用户提问，模型接着自言自语循环）。识别文本若正好是最近某句播报的
// 结尾，就当没听见——宁可漏一句（用户真想复述时再说一遍即可），
// 不能让音箱跟自己聊天。
func (s *Session) isEcho(text string) bool {
	n := normalizeEcho(text)
	nr := []rune(n)
	if len(nr) < 2 {
		return false
	}
	t := now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.spoken {
		if t.Sub(it.at) > echoWindow {
			continue
		}
		tr := []rune(normalizeEcho(it.text))
		if len(tr) < len(nr) {
			continue
		}
		suffix := tr[len(tr)-len(nr):]
		// 识别本就带错字，长一点的尾巴容一个错字
		tol := 0
		if len(nr) >= 4 {
			tol = 1
		}
		if echoDistance(suffix, nr) <= tol {
			return true
		}
	}
	return false
}

// normalizeEcho 去掉空白与标点，只留内容字符（比对回声用）。
func normalizeEcho(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '。', '，', '！', '？', '、', '.', ',', '!', '?', '：', ':', '…', '嗯', '啊', '呃':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// echoDistance 是逐字符的编辑距离（都很短，不需要优化）。
func echoDistance(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := cur[j-1] + 1
			if prev[j]+1 < m {
				m = prev[j] + 1
			}
			if prev[j-1]+cost < m {
				m = prev[j-1] + cost
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// ID 返回设备 id。
func (s *Session) ID() string { return s.id }

// Active 表示会话已被唤醒、正在等用户说话。
// 只有 listening 状态才该接受识别结果——否则家里任何对话都会被回复。
func (s *Session) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateListening
}

// State 返回当前状态。
func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// ProfileID 返回当前档案。
func (s *Session) ProfileID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.profile == nil {
		return ""
	}
	return s.profile.ID
}

// ExitWords 返回当前生效的退出词：档案配了就用档案的，否则用全局的。
func (s *Session) ExitWords() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.profile != nil && len(s.profile.ExitWords) > 0 {
		return s.profile.ExitWords
	}
	return s.mgr.ExitWords
}

// replyText 取"档案里配了就用档案的，否则用全局"的应答文案。
func (s *Session) replyText(profileText, globalText string) string {
	if t := strings.TrimSpace(profileText); t != "" {
		return t
	}
	return globalText
}

// speakOptions 返回本轮生效的播报参数（全局 → 档案覆盖）。
// 调用方不能持有 s.mu。
func (s *Session) speakOptions() SpeakOptions {
	s.mu.Lock()
	p := s.profile
	s.mu.Unlock()
	return s.mgr.speakOptionsOf(p)
}

// applyVoice 在本轮开始前把该档案的语速落到引擎上。
// 档案没配就恢复全局语速，避免上一个助手留下的语速一直生效。
func (s *Session) applyVoice() {
	s.mu.Lock()
	p := s.profile
	s.mu.Unlock()
	if p == nil {
		return
	}
	s.mgr.ApplySpeed(s.mgr.SpeedOf(p))
}

// idleTimeout 返回该档案的静音超时。
func (s *Session) idleTimeout() time.Duration {
	s.mu.Lock()
	p := s.profile
	s.mu.Unlock()
	return s.mgr.TimeoutOf(p)
}

// SayWakeReply 只播唤醒应答（不改变状态），文案取当前档案的，没配则用全局。
func (s *Session) SayWakeReply(ctx context.Context) string {
	// speakOptions() 内部要拿 s.mu，必须在上锁之前算好。
	// 之前写在 s.mu.Lock() 后面 → Go 的 Mutex 不可重入 → 自死锁，
	// 协程永远占着 s.mu，Sweep 拿着 m.mu 等它，后台保存就全卡住了。
	opt := s.speakOptions()
	s.mu.Lock()
	p, tgt := s.profile, s.tgt
	text := s.mgr.Replies.Wake
	if p != nil {
		text = s.replyText(p.WakeReply, text)
	}
	s.mu.Unlock()
	if strings.TrimSpace(text) == "" || tgt == nil {
		return ""
	}
	// 唤醒应答：不闭麦（否则吞掉紧跟着的提问开头）、不补"说完"音
	opt.NoMute = true
	opt.NoDone = true
	_ = s.mgr.sayCached(ctx, tgt, text, opt)
	s.noteSpoken(text)
	return text
}

// SayExitReply 播退出应答，文案取当前档案的，没配则用全局。
func (s *Session) SayExitReply(ctx context.Context) string {
	// 同上：不能在持 s.mu 时调 speakOptions()
	opt := s.speakOptions()
	s.mu.Lock()
	p, tgt := s.profile, s.tgt
	text := s.mgr.Replies.Exit
	if p != nil {
		text = s.replyText(p.ExitReply, text)
	}
	s.mu.Unlock()
	if strings.TrimSpace(text) == "" || tgt == nil {
		return ""
	}
	// 收尾语：播完不再补"说完了"提示音
	opt.NoDone = true
	_ = s.mgr.sayCached(ctx, tgt, text, opt)
	s.noteSpoken(text)
	return text
}

// Wake 处理唤醒：按唤醒词选档案，播应答语，进入聆听。
func (s *Session) Wake(ctx context.Context, keyword string) error {
	return s.wake(ctx, keyword, false)
}

// wake 是唤醒的公共实现。silent=true 时不播应答语。
func (s *Session) wake(ctx context.Context, keyword string, silent bool) error {
	p := s.mgr.Reg.Match(keyword)

	s.mu.Lock()
	// 正在播报时被唤醒 → 打断上一轮（用户明显想插话）
	if s.state == StateSpeaking && s.cancel != nil {
		s.cancel()
	}
	// 换档案时清空历史，避免把上一个助手的上下文带过去
	if s.profile == nil || s.profile.ID != p.ID {
		s.history = nil
	}
	s.profile = p
	s.state = StateListening
	s.lastActive = now()
	s.mu.Unlock()

	base := s.mgr.BaseCtx
	if base == nil {
		base = ctx
	}
	runCtx, cancel := context.WithCancel(base)
	s.mu.Lock()
	s.ctx = runCtx
	s.cancel = cancel
	s.mu.Unlock()

	s.mgr.log.Printf("唤醒 [%s] 唤醒词=%q → 档案=%s(%s) 模型=%s",
		s.id, keyword, p.ID, p.Name, p.LLM.Model)

	// 应答语：短、快，别让用户等（文案优先取档案）
	wakeText := s.replyText(p.WakeReply, s.mgr.Replies.Wake)
	if !silent && wakeText != "" {
		s.applyVoice() // 唤醒应答也要按该档案的语速念
		if err := s.mgr.sayCached(s.ctx, s.tgt, wakeText, s.speakOptions()); err != nil {
			s.mgr.log.Printf("[%s] 唤醒应答播放失败: %v", s.id, err)
		}
		s.noteSpoken(wakeText)
	}
	return nil
}

// WakeSilent 唤醒但不播应答语。
//
// 免唤醒模式下每句话都会走一次唤醒，再念"我在"就变成每次都多一句话，
// 这时候用提示音给反馈更合适。
func (s *Session) WakeSilent(ctx context.Context, keyword string) error {
	return s.wake(ctx, keyword, true)
}

// IsEcho 暴露回声判断给设备链路用：要在播"叮"和落"处理中"日志之前拦。
func (s *Session) IsEcho(text string) bool { return s.isEcho(text) }

// HandleText 处理一轮识别结果。
func (s *Session) HandleText(ctx context.Context, text string) (*Turn, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}

	// 先认一下是不是自己的回声：闭麦尾巴盖不住的混响会被识别成
	// "用户说了我们刚说的最后几个字"，接着就是自言自语循环。
	if s.isEcho(text) {
		s.mgr.log.Printf("[%s] 疑似听到自己的回声，忽略: %q", s.id, text)
		return nil, nil
	}

	s.mu.Lock()
	p := s.profile
	if p == nil {
		p = s.mgr.Reg.Fallback()
		s.profile = p
	}
	hist := s.history
	// 上下文保鲜期：隔太久就别再带上旧对话。
	// 用户隔半小时再说话时，把旧话题塞进提示词既费 token，
	// 又会让模型把上一轮的语境当成当前语境，答得莫名其妙。
	staleGap := time.Duration(0)
	if ttl := s.mgr.ContextTTL; ttl > 0 && !s.lastActive.IsZero() &&
		now().Sub(s.lastActive) > ttl && len(hist) > 0 {
		staleGap = now().Sub(s.lastActive)
		hist = nil
		s.history = nil
	}
	s.state = StateThinking
	s.lastActive = now()
	parent := ctx
	if s.cancel != nil {
		parent = s.ctx
	}
	s.mu.Unlock()

	// 本轮生效的语速（全局 + 档案覆盖）
	s.applyVoice()

	turn := &Turn{ProfileID: p.ID, Text: text, At: now()}
	start := now()
	if staleGap > 0 {
		s.mgr.log.Printf("[%s] 上下文已过期（隔了 %.0f 分钟 > %.0f 分钟），清空历史重新开始",
			s.id, staleGap.Minutes(), s.mgr.ContextTTL.Minutes())
	}

	// 退出词：不调模型，直接回应并结束
	if p.IsExit(text) {
		s.mu.Lock()
		s.state = StateIdle
		s.history = nil
		s.mu.Unlock()
		exitText := s.replyText(p.ExitReply, s.mgr.Replies.Exit)
		_ = s.mgr.sayCached(parent, s.tgt, exitText, s.speakOptions())
		s.noteSpoken(exitText)
		turn.Reply = "(退出)"
		turn.TotalMS = time.Since(start).Milliseconds()
		s.report(turn)
		return turn, nil
	}

	runner := s.mgr.runnerFor(p)

	sp := NewStreamSpeakerWith(parent, s.mgr.Speech, s.tgt, s.speakOptions())
	sp.onFirstAudio = func() {
		turn.FirstAudioMS = time.Since(start).Milliseconds()
	}

	var firstOnce sync.Once
	system := p.EffectiveSystemPrompt()
	if s.mgr.Context != nil {
		t0 := now()
		if extra := s.mgr.Context(parent, p, text); extra != "" {
			// 把预取内容打出来：判断"天气答错了"到底是没取到、取到了但模型没用，
			// 还是取到的就是错的——没有这行 Log 只能盲猜。
			s.mgr.log.Printf("[%s] 预取上下文(%.0fms)：%s", s.id,
				float64(time.Since(t0).Milliseconds()), strings.ReplaceAll(extra, "\n", " | "))
			system = system + "\n\n[已知信息]\n" + extra
		}
	}
	res, newHist, err := runner.RunWithSystem(parent, system, hist, text, func(d string) {
		firstOnce.Do(func() { turn.FirstDeltaMS = time.Since(start).Milliseconds() })
		sp.Write(d)
	})
	if cerr := sp.Close(); err == nil && cerr != nil {
		err = cerr
	}

	s.mu.Lock()
	s.history = trimHistory(newHist, s.mgr.historyLimit())
	s.state = StateListening
	s.lastActive = now()
	s.mu.Unlock()

	turn.Reply = res.Text
	turn.Rounds = res.Rounds
	turn.ToolRounds = res.ToolRounds
	turn.RoundMS = res.RoundMS
	turn.Sentences = sp.Spoken()
	turn.TotalMS = time.Since(start).Milliseconds()
	s.noteSpoken(res.Text)
	if err != nil {
		turn.Err = err.Error()
		// 让用户知道"这次没成功"，否则就是一片沉默，不知道是不是坏了
		// 出错多半就是因为 parent 被 cancel（超时/打断），
		// 再拿它去播"抱歉，出了点问题"必然静默——用户只能面对一片安静。
		// 这里换独立 ctx，保证"出错了"这句话一定说得出来。
		if s.mgr.Replies.Error != "" && turn.Rounds == 0 {
			ectx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = s.mgr.sayCached(ectx, s.tgt, s.mgr.Replies.Error, s.speakOptions())
			s.noteSpoken(s.mgr.Replies.Error)
			cancel()
		}
	}
	s.report(turn)
	return turn, err
}

// Interrupt 打断当前说话（用户又说了一句，或超时）。
func (s *Session) Interrupt() {
	s.mu.Lock()
	cancel := s.cancel
	s.state = StateIdle
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Idle 回到空闲并清空上下文。
func (s *Session) Idle() {
	s.mu.Lock()
	s.state = StateIdle
	s.history = nil
	s.profile = nil
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Touch 刷新活跃时间，返回是否已超时。
func (s *Session) Expired(timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateIdle {
		return true
	}
	return now().Sub(s.lastActive) > timeout
}

func (s *Session) report(t *Turn) {
	t.Device = s.id
	if s.mgr.OnTurn != nil {
		s.mgr.OnTurn(t)
	}
	s.mgr.log.Printf("[%s] 档案=%s 轮数=%d 各轮=%vms 首字=%dms 总=%dms 句数=%d 问=%q 答=%q",
		s.id, t.ProfileID, t.Rounds, t.RoundMS, t.FirstDeltaMS, t.TotalMS, t.Sentences,
		t.Text, truncate(t.Reply, 40))
}

func trimHistory(h []llm.Message, max int) []llm.Message {
	if len(h) <= max {
		return h
	}
	// 保留最近 max 条，且保证不以 tool 消息开头（否则模型会困惑）
	out := h[len(h)-max:]
	for len(out) > 0 && out[0].Role == "tool" {
		out = out[1:]
	}
	return out
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
