package audio

// Service 是运行时的 TTS 路由层。
//
// 解决的问题：以前"用哪个 TTS"是引擎的构造参数，改一次要重建整个引擎
// （ASR/VAD/KWS 跟着重载，几秒 + 几百 MB）。而产品的实际需求是
// "同一个音箱，喊小美用温柔女声、喊老张用浑厚男声，还能随时在后台换平台"，
// 这必须做到每轮对话自己决定，不能靠重启。
//
// Service 的三件事：
//  1. 按 VoiceChoice 解析出真正的 (provider, params, voice, speed)；
//  2. 维护一个实例池：provider+params 相同的请求复用同一个实例
//     （本地模型不用重复加载，云端连接不用重复握手）；
//  3. 把音色/语速作为请求参数传下去，而不是改实例状态
//     （并发两路对话不会互相串音色）。
//
// 与引擎的关系：全局 provider 的实例就装在引擎里，Service 直接借用，
// 不重复加载；只有"某个档案用了和全局不同的平台"时才另开实例。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrTTSTimeout 表示合成超时（单独定义，方便上层给不同的提示音）。
var ErrTTSTimeout = errors.New("audio: 语音合成超时")

// Service 是 Speaker 的默认实现，也是后台的 TTS 管理入口。
type Service struct {
	mu sync.Mutex

	// 当前引擎里的 TTS（全局 provider 的实例），可为 nil。
	eng     Engine
	engProv string            // 引擎装的是哪个 provider
	engCfg  map[string]string // 引擎装的那份参数（用于判断能否复用）

	defProv string            // 默认 provider（配置里的全局选择）
	defCfg  map[string]string // 默认 provider 的参数
	// cfgs 是"每个平台一份参数"。档案把助手切到另一个平台时，
	// 用这里的配置打底 —— 密钥是账号级的，不该逼用户在每个助手里抄一遍。
	cfgs map[string]map[string]string

	live  map[string]*liveTTS
	order []string // 近似 LRU：越靠后越新

	limit int
	speed float32

	// voiceCache 缓存"在线拉取到的完整音色清单"。
	// 不写进 settings.json：Edge 有 300+ 个音色，塞进配置会让配置文件变成
	// 十几 KB 的噪音，而且每次打开后台都要整份传一遍。这里落一个小 json 文件。
	voiceCacheDir string
	voiceCache    map[string]voiceCacheEntry

	// Logf 可选，接上日志方便排查"为什么这轮声音变了"。
	Logf func(format string, args ...any)
}

type liveTTS struct {
	syn  Synthesizer
	key  string
	used time.Time
}

// NewService 构造路由层。defProvider 为空时按 "sherpa" 处理（与装配层的约定一致）。
func NewService(defProvider string, defCfg map[string]string) *Service {
	s := &Service{
		defProv:    strings.ToLower(strings.TrimSpace(defProvider)),
		defCfg:     cloneCfg(defCfg),
		cfgs:       map[string]map[string]string{},
		live:       make(map[string]*liveTTS, 4),
		voiceCache: map[string]voiceCacheEntry{},
		limit:      8,
		speed:      1,
	}
	if s.defProv == "" {
		s.defProv = "sherpa"
	}
	if len(defCfg) > 0 {
		s.cfgs[s.defProv] = cloneCfg(defCfg)
	}
	return s
}

// SetDefault 热更新"全局用谁"。provider 变了不必重建引擎：
// 新平台的实例会懒加载，老实例留在池里等 LRU 淘汰。
func (s *Service) SetDefault(provider string, cfg map[string]string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "sherpa"
	}
	s.mu.Lock()
	s.defProv = provider
	s.defCfg = cloneCfg(cfg)
	if s.cfgs == nil {
		s.cfgs = map[string]map[string]string{}
	}
	if len(cfg) > 0 {
		s.cfgs[provider] = cloneCfg(cfg)
	}
	s.mu.Unlock()
}

// SetProviderConfigs 更新"每个平台一份参数"的全量快照。
// 档案切平台时靠它拿到那个平台的密钥，用户只需配一次。
func (s *Service) SetProviderConfigs(cfgs map[string]map[string]string) {
	next := make(map[string]map[string]string, len(cfgs))
	for k, v := range cfgs {
		next[strings.ToLower(strings.TrimSpace(k))] = cloneCfg(v)
	}
	s.mu.Lock()
	s.cfgs = next
	s.mu.Unlock()
}

// SetEngine 绑定当前引擎。provider/cfg 是"这个引擎是用哪套配置装出来的"，
// 用来判断某次请求能不能直接复用引擎里的实例。
func (s *Service) SetEngine(eng Engine, provider string, cfg map[string]string) {
	s.mu.Lock()
	// 引擎换了，之前"借用引擎"的缓存判断全部作废
	s.eng = eng
	s.engProv = strings.ToLower(strings.TrimSpace(provider))
	s.engCfg = cloneCfg(cfg)
	s.mu.Unlock()
}

// SetSpeed 设置全局语速。注意它只是兜底值：每轮对话会用自己的 VoiceChoice.Speed，
// 档案里配了语速就以档案为准。
func (s *Service) SetSpeed(v float32) {
	if v <= 0 {
		v = 1
	}
	s.mu.Lock()
	s.speed = v
	s.mu.Unlock()
}

// Speed 返回全局语速。
func (s *Service) Speed() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speed
}

// Status 汇总当前可用的合成能力（后台"引擎状态"里显示）。
func (s *Service) Status() string {
	s.mu.Lock()
	eng, engProv := s.eng, s.engProv
	defProv := s.defProv
	n := len(s.live)
	s.mu.Unlock()

	var parts []string
	if eng != nil {
		parts = append(parts, fmt.Sprintf("全局=%s", engProv))
	}
	if defProv != "" && defProv != engProv {
		parts = append(parts, fmt.Sprintf("默认=%s(未加载)", defProv))
	}
	if n > 0 {
		parts = append(parts, fmt.Sprintf("备用实例=%d", n))
	}
	if len(parts) == 0 {
		return "off"
	}
	return strings.Join(parts, " ")
}

// Available 表示至少有一路能合成。
func (s *Service) Available() bool {
	s.mu.Lock()
	eng, defProv, n := s.eng, s.defProv, len(s.live)
	s.mu.Unlock()
	if eng != nil {
		if h, ok := eng.(interface{ HasTTS() bool }); ok {
			if h.HasTTS() {
				return true
			}
		} else {
			return true
		}
	}
	if n > 0 {
		return true
	}
	return defProv != "none" && HasTTSProvider(defProv)
}

// Synthesize 合成一句话。v 里的空字段一律跟随全局。
//
// 返回 PCM（16bit/mono）与该段音频的采样率。采样率必须逐次返回：
// 不同平台/音色的采样率不一样（豆包 24k、Edge 24k、本地 VITS 8k/22k），
// 用实例上的固定值会让播放端按错误的速率播。
func (s *Service) Synthesize(ctx context.Context, text string, v VoiceChoice) ([]byte, int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, 0, nil
	}
	plan := s.Resolve(v)
	plan.Text = text
	if plan.Provider == "none" {
		return nil, 0, fmt.Errorf("%w: TTS 已关闭", ErrCapability)
	}

	// 快路径：请求的就是引擎里装的那套 → 直接用引擎，零额外开销。
	if eng, ok := s.engineFor(plan); ok {
		pcm, err := synthVia(ctx, eng, plan)
		if err != nil {
			return nil, 0, err
		}
		return pcm, rateOf(eng, plan), nil
	}

	syn, err := s.instance(plan)
	if err != nil {
		return nil, 0, err
	}
	pcm, err := synthVia(ctx, syn, plan)
	if err != nil {
		return nil, 0, err
	}
	return pcm, rateOf(syn, plan), nil
}

// Plan 是解析后的执行计划（导出给后台"当前生效"展示与日志）。
type Plan struct {
	Provider string
	Voice    string
	Speed    float32
	Params   map[string]string
	Text     string
}

// Key 是实例缓存的键：只含 provider 与参数（不含音色，音色是请求参数）。
func (p Plan) Key() string {
	return cacheKeyOf(p.Provider, p.Params)
}

// Resolve 把 VoiceChoice 与全局配置合并成一份确定的执行计划。
//
// 合并规则（顺序很重要，写在这里免得各处再猜一遍）：
//  1. provider 空 → 全局 provider；
//  2. 参数：先取"这个平台自己的全局配置"打底，再用档案参数覆盖。
//     平台级配置是账号级的（一个豆包 key 服务所有助手），档案只在
//     确实要用另一个账号时才填——不然换一次 key 要改十几处。
//  3. 音色：档案 > 全局（仅当 provider 一致）> 参数里的 voice > provider 默认；
//  4. 语速：档案 > 全局 > 1.0。
func (s *Service) Resolve(v VoiceChoice) Plan {
	s.mu.Lock()
	defProv, defCfg, globalSpeed := s.defProv, s.defCfg, s.speed
	base := cloneCfg(s.cfgs[strings.ToLower(strings.TrimSpace(v.Provider))])
	s.mu.Unlock()

	p := Plan{Provider: strings.ToLower(strings.TrimSpace(v.Provider))}
	if p.Provider == "" {
		p.Provider = defProv
	}
	if p.Provider == defProv && len(base) == 0 {
		base = cloneCfg(defCfg)
	}
	p.Params = mergeCfg(base, v.Params)
	p.Voice = strings.TrimSpace(v.Voice)
	if p.Voice == "" && p.Provider == defProv {
		// 全局音色：显式字段优先，其次兼容老配置里塞在参数里的 voice
		p.Voice = strings.TrimSpace(p.Params["voice"])
	}
	if p.Voice == "" {
		p.Voice = defaultVoiceOf(p.Provider, p.Params)
	}
	p.Speed = v.Speed
	if p.Speed <= 0 {
		p.Speed = globalSpeed
	}
	if p.Speed <= 0 {
		p.Speed = 1
	}
	return p
}

// defaultVoiceOf 取 provider 声明里的默认音色，其次参数里写死的。
func defaultVoiceOf(provider string, cfg map[string]string) string {
	if v := strings.TrimSpace(cfg["voice"]); v != "" {
		return v
	}
	if p, ok := FindTTSProvider(provider); ok {
		for _, f := range p.Fields {
			if f.Key == "voice" && f.Default != "" {
				return f.Default
			}
		}
		if len(p.Voices) > 0 {
			return p.Voices[0].ID
		}
	}
	return ""
}

// engineFor 判断这次请求能否直接用引擎里的实例。
func (s *Service) engineFor(plan Plan) (Engine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil || s.engProv != plan.Provider {
		return nil, false
	}
	if !sameCfg(s.engCfg, plan.Params) {
		return nil, false
	}
	if h, ok := s.eng.(interface{ HasTTS() bool }); ok && !h.HasTTS() {
		return nil, false
	}
	return s.eng, true
}

// instance 取（或创建）一个 provider 实例。
func (s *Service) instance(plan Plan) (Synthesizer, error) {
	key := plan.Key()

	s.mu.Lock()
	if it := s.live[key]; it != nil {
		it.used = time.Now()
		s.touchLocked(key)
		syn := it.syn
		s.mu.Unlock()
		return syn, nil
	}
	s.mu.Unlock()

	syn, err := NewSynthesizer(plan.Provider, TTSOptions{
		Provider: plan.Provider, Speed: plan.Speed, Params: plan.Params,
	})
	if err != nil {
		return nil, err
	}
	if ss, ok := syn.(SpeedSetter); ok {
		ss.SetSpeed(plan.Speed)
	}

	s.mu.Lock()
	if old := s.live[key]; old != nil {
		// 并发下别人先建好了：留一个，关掉多的
		s.mu.Unlock()
		_ = syn.Close()
		return old.syn, nil
	}
	s.live[key] = &liveTTS{syn: syn, key: key, used: time.Now()}
	s.order = append(s.order, key)
	evicted := s.evictLocked()
	s.mu.Unlock()

	for _, e := range evicted {
		_ = e.Close()
	}
	if s.Logf != nil {
		s.Logf("TTS 实例已创建: provider=%s voice=%s", plan.Provider, plan.Voice)
	}
	return syn, nil
}

// touchLocked 把 key 挪到队尾（最近使用）。
func (s *Service) touchLocked(key string) {
	for i, k := range s.order {
		if k == key {
			s.order = append(append(s.order[:i], s.order[i+1:]...), key)
			return
		}
	}
}

// evictLocked 淘汰超额实例，返回需要关闭的那些（在锁外关，避免 Close 卡住别人）。
func (s *Service) evictLocked() []Synthesizer {
	var out []Synthesizer
	for len(s.order) > s.limit {
		victim := s.order[0]
		s.order = s.order[1:]
		if it := s.live[victim]; it != nil {
			delete(s.live, victim)
			out = append(out, it.syn)
		}
	}
	return out
}

// Close 释放所有备用实例。引擎持有的那份由引擎自己关。
func (s *Service) Close() error {
	s.mu.Lock()
	live := s.live
	s.live = make(map[string]*liveTTS, 4)
	s.order = nil
	s.mu.Unlock()
	for _, it := range live {
		_ = it.syn.Close()
	}
	return nil
}

// ---------- 后台用：供应商 / 音色 / 复刻 ----------

// Voices 返回某供应商的音色清单：内置精选 + 手工登记 +（缓存的）在线完整清单。
//
// refresh=true 时强制重新拉取（后台的「刷新音色」按钮）。
// 拉取失败不报错：退回已有清单并把错误带出去——音色列表打不开不该让整页崩掉。
//
// 为什么要缓存：Edge 有 300+ 个音色，每次打开后台都拉一遍要 1~2 秒，
// 而音色清单一个月都不会变。缓存 7 天，写成一个独立小文件（不进 settings.json）。
func (s *Service) Voices(ctx context.Context, provider string, cfg map[string]string, refresh bool) ([]Voice, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	p, ok := FindTTSProvider(provider)
	if !ok {
		return nil, fmt.Errorf("audio: 未知的 TTS 供应商 %q", provider)
	}
	out := append([]Voice(nil), p.Voices...)
	// 用户在配置里手工登记的音色（语音清单 / 复刻音色）：
	// 平台没有"列出音色"接口、或内置清单只是精选时的兜底。
	out = append(out, customVoicesFromParams(cfg)...)

	var firstErr error
	if p.Clone != nil && p.Clone.List != nil {
		if cs, err := p.Clone.List(ctx, cfg); err == nil {
			out = append(out, cs...)
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if p.Discover != nil {
		if ds, err := s.discover(ctx, provider, cfg, refresh); err == nil {
			out = append(out, ds...)
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return sortedVoices(out), firstErr
}

// voiceCacheEntry 是一份缓存下来的在线音色清单。
type voiceCacheEntry struct {
	At     time.Time `json:"at"`
	Voices []Voice   `json:"voices"`
}

// voiceCacheTTL 是缓存有效期。音色库变化很慢，7 天足够；
// 想立刻看到新音色点「刷新音色」即可（refresh=true 绕过缓存）。
const voiceCacheTTL = 7 * 24 * time.Hour

// SetVoiceCacheDir 指定缓存目录（一般是数据目录下的 tts-voices）。
// 传空表示只缓存到内存，重启即失效。
func (s *Service) SetVoiceCacheDir(dir string) {
	s.mu.Lock()
	s.voiceCacheDir = dir
	s.mu.Unlock()
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
}

// discover 取在线音色清单，优先读缓存。
func (s *Service) discover(ctx context.Context, provider string, cfg map[string]string, refresh bool) ([]Voice, error) {
	p, _ := FindTTSProvider(provider)
	if p.Discover == nil {
		return nil, nil
	}
	if !refresh {
		if v, ok := s.cachedVoices(provider); ok {
			return v, nil
		}
	}
	// 拉取失败时退回旧缓存（哪怕过期）：有旧清单总比只有一个内置清单好
	got, err := p.Discover(ctx, cfg)
	if err != nil {
		if v, ok := s.cachedVoices(provider); ok {
			return v, nil
		}
		return nil, err
	}
	if len(got) == 0 {
		return nil, nil
	}
	s.storeVoices(provider, got)
	return got, nil
}

func (s *Service) cachedVoices(provider string) ([]Voice, bool) {
	s.mu.Lock()
	e, ok := s.voiceCache[provider]
	dir := s.voiceCacheDir
	s.mu.Unlock()
	if !ok && dir != "" {
		// 冷启动：从磁盘读一次
		raw, err := os.ReadFile(filepath.Join(dir, provider+".json"))
		if err == nil {
			var disk voiceCacheEntry
			if json.Unmarshal(raw, &disk) == nil && len(disk.Voices) > 0 {
				e, ok = disk, true
				s.mu.Lock()
				s.voiceCache[provider] = e
				s.mu.Unlock()
			}
		}
	}
	// 过期也照样返回：音色清单过期远好过"下拉框忽然只剩 12 个"。
	// 想拿新的点「刷新音色」（refresh=true）。
	if !ok || len(e.Voices) == 0 {
		return nil, false
	}
	return e.Voices, true
}

func (s *Service) storeVoices(provider string, voices []Voice) {
	e := voiceCacheEntry{At: time.Now(), Voices: voices}
	s.mu.Lock()
	s.voiceCache[provider] = e
	dir := s.voiceCacheDir
	s.mu.Unlock()
	if dir == "" {
		return
	}
	if b, err := json.Marshal(e); err == nil {
		_ = os.WriteFile(filepath.Join(dir, provider+".json"), b, 0o644)
	}
}

// CloneSubmit 提交一次声音复刻。
func (s *Service) CloneSubmit(ctx context.Context, provider string, cfg map[string]string, req CloneRequest) (CloneState, error) {
	p, ok := FindTTSProvider(provider)
	if !ok {
		return CloneState{}, fmt.Errorf("audio: 未知的 TTS 供应商 %q", provider)
	}
	if p.Clone == nil || p.Clone.Upload == nil {
		return CloneState{}, fmt.Errorf("audio: 供应商 %q 不支持声音复刻", provider)
	}
	return p.Clone.Upload(ctx, cfg, req)
}

// CloneQuery 查询复刻状态。
func (s *Service) CloneQuery(ctx context.Context, provider string, cfg map[string]string, speakerID string) (CloneState, error) {
	p, ok := FindTTSProvider(provider)
	if !ok {
		return CloneState{}, fmt.Errorf("audio: 未知的 TTS 供应商 %q", provider)
	}
	if p.Clone == nil || p.Clone.Query == nil {
		return CloneState{}, fmt.Errorf("audio: 供应商 %q 不支持声音复刻", provider)
	}
	return p.Clone.Query(ctx, cfg, speakerID)
}

// ---------- 内部工具 ----------

// synthVia 调一个 Synthesizer，优先用"带音色/语速"的接口。
func synthVia(ctx context.Context, syn any, plan Plan) ([]byte, error) {
	opt := SynthOptions{Text: plan.Text, Voice: plan.Voice, Speed: plan.Speed}
	if vs, ok := syn.(VoiceSynthesizer); ok {
		return vs.SynthesizeWith(ctx, opt)
	}
	switch v := syn.(type) {
	case Synthesizer:
		return v.Synthesize(ctx, opt.Text)
	case Engine:
		return v.Synthesize(ctx, opt.Text)
	}
	return nil, fmt.Errorf("%w: TTS", ErrCapability)
}

// rateOf 取实例的采样率；实现不来就按 24k（云端 TTS 的主流值）。
func rateOf(syn any, plan Plan) int {
	if v, ok := syn.(interface{ TTSRate() int }); ok {
		if r := v.TTSRate(); r > 0 {
			return r
		}
	}
	return 24000
}

// cacheKeyOf 由 provider 与参数算出实例键。
// 参数按 key 排序后参与哈希，保证 map 迭代顺序不影响结果。
func cacheKeyOf(provider string, cfg map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "prov=%s\x00", provider)
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\x00", k, cfg[k])
	}
	return provider + ":" + hex.EncodeToString(h.Sum(nil))[:16]
}

// sameCfg 判断两份配置是否等价（用于"能否复用引擎实例"）。
func sameCfg(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// mergeCfg 以 base 打底、over 覆盖（over 的空值视为"不覆盖"，方便表单留空=继承）。
func mergeCfg(base, over map[string]string) map[string]string {
	out := cloneCfg(base)
	for k, v := range over {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func cloneCfg(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
