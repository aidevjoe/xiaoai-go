package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
	"xiaoai-go/internal/server/dialog"
	"xiaoai-go/internal/server/store"
	"xiaoai-go/internal/server/tools"
)

// turnLog 保存最近的对话记录，并向所有 SSE 订阅者广播。
type turnLog struct {
	mu    sync.Mutex
	items []*dialog.Turn
	max   int
	subs  map[chan []byte]struct{}
}

func newTurnLog(max int) *turnLog {
	if max <= 0 {
		max = 200
	}
	return &turnLog{max: max, subs: make(map[chan []byte]struct{})}
}

// SetMax 调整保留条数（后台改完立刻生效）。
func (l *turnLog) SetMax(n int) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	l.max = n
	if len(l.items) > n {
		l.items = l.items[len(l.items)-n:]
	}
	l.mu.Unlock()
}

// Clear 清空记录。
func (l *turnLog) Clear() {
	l.mu.Lock()
	l.items = nil
	l.mu.Unlock()
}

// add 追加一条并广播。
//
// 注意：一句话进来时会先写一条 Pending（只有"问"，没有"答"），
// 模型答完之后再写最终结果。这里要把它就地替换掉，
// 否则同一次提问会出现两条记录。
func (l *turnLog) add(t *dialog.Turn) {
	l.mu.Lock()
	replaced := false
	if !t.Pending {
		for i := len(l.items) - 1; i >= 0 && i > len(l.items)-20; i-- {
			it := l.items[i]
			if it.Pending && it.Device == t.Device && it.Text == t.Text {
				l.items[i] = t
				replaced = true
				break
			}
		}
	}
	if !replaced {
		l.items = append(l.items, t)
	}
	if len(l.items) > l.max {
		l.items = l.items[len(l.items)-l.max:]
	}
	subs := make([]chan []byte, 0, len(l.subs))
	for ch := range l.subs {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	if b, err := json.Marshal(map[string]any{"type": "turn", "data": t}); err == nil {
		for _, ch := range subs {
			select {
			case ch <- b:
			default: // 订阅者读太慢就丢，不能阻塞对话
			}
		}
	}
}

// list 返回最近 n 条（新的在后）。
func (l *turnLog) list(n int) []*dialog.Turn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.items) {
		n = len(l.items)
	}
	out := make([]*dialog.Turn, n)
	copy(out, l.items[len(l.items)-n:])
	return out
}

func (l *turnLog) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch
}

func (l *turnLog) unsubscribe(ch chan []byte) {
	l.mu.Lock()
	delete(l.subs, ch)
	l.mu.Unlock()
	close(ch)
}

// broadcast 推送非对话类事件（状态、配置变更）。
func (l *turnLog) broadcast(kind string, v any) {
	b, err := json.Marshal(map[string]any{"type": kind, "data": v})
	if err != nil {
		return
	}
	l.mu.Lock()
	subs := make([]chan []byte, 0, len(l.subs))
	for ch := range l.subs {
		subs = append(subs, ch)
	}
	l.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- b:
		default:
		}
	}
}

// ---- 配置热更新 ----

// applySettings 应用全部配置。校验通过才改内存，避免把系统配成半截状态。
func (s *server) applySettings(st store.Settings) error {
	reg, err := agent.NewRegistry(st.Profiles)
	if err != nil {
		return err
	}
	s.reg.Store(reg)
	if s.dm != nil {
		s.dm.SetRegistry(reg)
	}

	s.applyTools(st)
	// 工具注册表可能被整体替换过；对话框里缓存了旧指针（runner 里也缓存了），
	// 不同步的话"后台改工具开关"要重启才生效。
	if s.dm != nil {
		s.dm.SetTools(s.toolsSnapshot())
	}

	// 语速可以原地改，不必重建 TTS 模型（重建要几秒 + 几百 MB 内存）
	// 走引用计数：这里可能正好撞上"重载引擎"，不能碰已 Close 的 C 对象。
	if eng, release, ok := s.acquireEngine(); ok {
		if ss, isSpeed := eng.(audio.SpeedSetter); isSpeed {
			ss.SetSpeed(st.Voice.Speed)
		}
		release()
	}

	s.setConfig(st)
	s.turns.SetMax(st.Server.KeepTurns)

	if s.dm != nil {
		s.applyDialogConfig(st)
	}
	// 分段参数变了要重建管道
	s.resetDeviceAudio()
	return nil
}

// applyTools 按配置重建工具集（HA 连接参数变了才重建）。
func (s *server) applyTools(st store.Settings) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()

	need := s.tools == nil || s.haURL != st.Tools.HAURL || s.haToken != st.Tools.HAToken
	if need {
		var ha *tools.HA
		if st.Tools.HAURL != "" && st.Tools.HAToken != "" {
			ha = tools.NewHA(st.Tools.HAURL, st.Tools.HAToken)
		}
		s.tools = tools.NewRegistry(ha)
		s.haURL, s.haToken = st.Tools.HAURL, st.Tools.HAToken
	}
	s.tools.SetEnabled(st.Tools.Enabled)

	// 联网搜索与天气：配置变了才重建（重建会丢掉缓存，天气缓存有 10 分钟价值）。
	webSig := fmt.Sprintf("%s|%s|%s|%d|%d|%v|%v",
		st.Tools.Search.Provider, st.Tools.Search.APIKey, st.Tools.Search.BaseURL,
		st.Tools.Search.MaxResults, st.Tools.Search.TimeoutMS,
		st.Tools.Search.Always, st.Tools.Search.Keywords)
	if webSig != s.webSig {
		var ws *tools.WebSearch
		if st.Tools.Search.Enabled {
			ws = tools.NewWebSearch(tools.SearchOptions{
				Provider:   st.Tools.Search.Provider,
				APIKey:     st.Tools.Search.APIKey,
				BaseURL:    st.Tools.Search.BaseURL,
				MaxResults: st.Tools.Search.MaxResults,
				Timeout:    time.Duration(st.Tools.Search.TimeoutMS) * time.Millisecond,
				Always:     st.Tools.Search.Always,
				Keywords:   st.Tools.Search.Keywords,
			})
		}
		s.tools.SetWeb(ws)
		s.webSig = webSig
	}

	wxSig := fmt.Sprintf("%v|%s|%s|%s|%d|%d",
		st.Tools.Weather.Enabled, st.Tools.Weather.Provider, st.Tools.Weather.APIKey,
		st.Tools.Weather.Location, st.Tools.Weather.Days, st.Tools.Weather.TimeoutMS)
	if wxSig != s.wxSig {
		w := tools.NewWeather(tools.WeatherOptions{
			Provider: st.Tools.Weather.Provider,
			APIKey:   st.Tools.Weather.APIKey,
			Location: st.Tools.Weather.Location,
			Days:     st.Tools.Weather.Days,
			Timeout:  time.Duration(st.Tools.Weather.TimeoutMS) * time.Millisecond,
		})
		// ha 直接读实体，不需要客户端；qweather / search 都要带配置。
		if !st.Tools.Weather.Enabled || st.Tools.Weather.Provider == "ha" {
			w = nil
		}
		s.tools.SetWeather(w, st.Tools.Weather.Enabled)
		// 城市/来源变了，缓存作废；不主动预热——后台定时搜会白烧搜索额度
		s.tools.ResetWeather()
		s.wxSig = wxSig
	}

	// 自定义工具：配置变了才重建（先摘旧的再注册新的，避免同名残留）。
	// 用签名而不是每次都重注册：Register 是覆盖式的，但反复重建会让
	// 对话路径上持有旧 runner 的会话看到"工具忽然消失又出现"。
	customSig := fmt.Sprintf("%+v", st.Tools.Custom)
	if customSig != s.customSig {
		for _, n := range s.customNames {
			s.tools.Unregister(n)
		}
		s.customNames = s.customNames[:0]
		for _, e := range s.tools.RegisterCustom(st.Tools.Custom) {
			log.Printf("自定义工具未启用: %v", e)
		}
		if s.tools != nil {
			for _, c := range st.Tools.Custom {
				if n := strings.TrimSpace(c.Name); n != "" {
					s.customNames = append(s.customNames, n)
				}
			}
		}
		s.customSig = customSig
	}

	if s.dm != nil {
		s.dm.Tools = s.tools
	}
}

// applyDialogConfig 把对话相关参数灌进 dialog.Manager。
func (s *server) applyDialogConfig(st store.Settings) {
	if s.dm == nil {
		return
	}
	s.dm.ExitWords = st.Dialog.ExitWords
	if st.Dialog.TimeoutSec > 0 {
		s.dm.Timeout = time.Duration(st.Dialog.TimeoutSec) * time.Second
	}
	s.dm.ContextTTL = time.Duration(st.Dialog.ContextTTLMin) * time.Minute
	s.dm.MaxRounds = st.Dialog.MaxRounds
	s.dm.MaxHistory = st.Dialog.MaxHistory
	s.dm.Speak = dialog.SpeakOptions{
		MaxSentenceRunes: st.Voice.MaxSentenceRunes,
		MaxSentences:     st.Dialog.MaxSentences,
		StripEmoji:       st.Voice.StripEmoji,
		DeviceTTS:        st.Voice.Backend == "device",
	}
	s.dm.Replies = dialog.Replies{
		Wake:  st.Voice.WakeReply,
		Exit:  st.Voice.ExitReply,
		Error: st.Voice.ErrorReply,
	}
	// 全局语速记在这里：档案可以覆盖它，换回没配语速的档案时要能恢复。
	s.dm.GlobalSpeed = st.Voice.Speed
	s.dm.ApplySpeed(st.Voice.Speed)

	// 全局音色：供应商 + 音色 + 该供应商的参数。档案只覆盖自己配的项。
	prov := st.Engine.TTSProvider
	if prov == "" {
		prov = "sherpa"
	}
	s.dm.GlobalVoice = audio.VoiceChoice{
		Provider: prov,
		Voice:    st.Engine.TTSVoice,
		Params:   st.Engine.TTSProviders[prov],
		Speed:    st.Voice.Speed,
	}
	if s.speech != nil {
		s.speech.SetDefault(prov, st.Engine.TTSProviders[prov])
		// 每个平台的参数都灌进去：助手档案切到别的平台时，凭据从这里取，
		// 用户只需要在「引擎与模型」里把每个平台配一次。
		s.speech.SetProviderConfigs(st.Engine.TTSProviders)
		s.speech.SetSpeed(st.Voice.Speed)
	}
	// 语音入口必须是路由层：否则档案里选的音色不生效（引擎只有一个音色）。
	if s.speech != nil {
		s.dm.SetSpeech(s.speech)
	}
	s.dm.Context = func(ctx context.Context, p *agent.Profile, text string) string {
		s.toolsMu.RLock()
		tl := s.tools
		s.toolsMu.RUnlock()
		return tl.Context(ctx, p, text)
	}
}

// deviceOptions 从配置生成管道参数。
func (s *server) deviceOptions() deviceOptions {
	st := s.config()
	opt := deviceOptions{
		MinSpeech:    time.Duration(st.Dialog.MinSpeechMS) * time.Millisecond,
		MinSilence:   time.Duration(st.Dialog.SilenceMS) * time.Millisecond,
		PreRoll:      time.Duration(st.Dialog.PreRollMS) * time.Millisecond,
		MaxUtterance: time.Duration(st.Dialog.MaxUtteranceMS) * time.Millisecond,
	}
	if st.Server.SaveAudio {
		dir := st.Server.AudioDir
		if dir == "" && s.store != nil {
			dir = filepath.Join(filepath.Dir(s.store.Path()), "audio")
		}
		opt.AudioDir = dir
	}
	return opt
}

// resetDeviceAudio 丢掉旧管道，让下次推流时按新参数重建。
// 旧管道必须显式停掉：它的消费协程和引擎引用都要归还，否则每改一次配置
// 就泄漏一条协程，并且新管道和旧管道会同时抢 ASR/TTS 的模型锁。
func (s *server) resetDeviceAudio() {
	s.devMu.Lock()
	old := s.devAudio
	s.devAudio = make(map[string]*deviceAudio, len(old))
	s.devMu.Unlock()

	ids := make([]string, 0, len(old))
	for id, d := range old {
		ids = append(ids, id)
		d.stop(deviceStopTimeout)
	}
	for _, id := range ids {
		s.startDeviceAudio(id)
	}
}

// deviceStopTimeout 停管道时的等待上限：正常一两条帧就退出了，
// 卡住多半是正在跑一次推理，等太久会拖住后台保存配置的请求。
const deviceStopTimeout = 3 * time.Second

// ---- HTTP 处理 ----

// handleSettingsGet 返回当前配置。
func (s *server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, 503, map[string]any{"error": "配置未加载"})
		return
	}
	writeJSON(w, 200, s.store.Get())
}

// handleSettingsPut 保存配置。校验不通过不落盘，避免把系统配坏。
func (s *server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, 503, map[string]any{"error": "配置未加载"})
		return
	}
	var incoming store.Settings
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		writeJSON(w, 400, map[string]any{"error": "JSON 解析失败: " + err.Error()})
		return
	}
	// 先做"能不能跑"的校验，再落盘
	if err := incoming.Normalize(); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}

	old := s.store.Get()
	engineChanged := !reflect.DeepEqual(old.Engine, incoming.Engine)
	_, oldRev := keywordWords(old)
	_, newRev := keywordWords(incoming)

	if err := s.applySettings(incoming); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.Save(incoming); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	// 唤醒词变了立刻推给在线设备：让"后台改词"和"音箱文件"不会长期不一致
	kwsDirty := oldRev != newRev
	if kwsDirty {
		go s.syncKeywords(context.Background(), "")
		// 服务端 KWS 的 keywords.txt 是构造 keyword spotter 时读进去的，
		// 必须重建引擎才生效。以前只靠前端提示"点重载"，实际没人点，
		// 表现就是"改了唤醒词喊不醒、重启服务端就好了"。这里直接自动重载。
		if incoming.Engine.KWSEnabled && !*flagNoEngine {
			go s.autoReloadEngine()
		}
	}

	s.turns.broadcast("settings", map[string]any{"saved": true})
	// 唤醒词变了要连带重建引擎：客户端那份 wakewords.txt 可以热同步，
	// 但服务端 KWS 的 keywords.txt 是构造 keyword spotter 时读进去的，
	// 不重建引擎就一直是旧唤醒词。
	engineDirty := engineChanged || (kwsDirty && incoming.Engine.KWSEnabled)
	writeJSON(w, 200, map[string]any{
		"ok": true,
		// 引擎参数（路径/线程/阈值/唤醒词）改了要重载模型才生效，
		// 提示前端弹一个"立即重载"按钮，而不是静默假装生效
		"engine_dirty": engineDirty,
		"kws_dirty":    kwsDirty,
	})
}

// handleEngineReload 按当前配置重新加载模型。
func (s *server) handleEngineReload(w http.ResponseWriter, r *http.Request) {
	if *flagNoEngine {
		writeJSON(w, 409, map[string]any{"error": "服务以 -no-engine 启动，引擎被禁用"})
		return
	}
	st := s.config()
	start := time.Now()
	eng, ttsProv, err := s.loadEngine(st)
	if err != nil {
		// 保留正在工作的引擎：填错一个模型路径不该让音箱当场失声，
		// 只需要把错误记下来、让后台看到「当前仍在用上一版模型」。
		s.setEngineErr(err)
		writeJSON(w, 500, map[string]any{"error": err.Error(), "kept_old": s.getEngine() != nil})
		return
	}
	// 引用计数接管旧引擎的关闭：正在跑的合成/识别用完才真正 Close，
	// 不再"睡 2 秒猜它跑完了没有"。installEngine 内部会重建设备管道。
	s.installEngineWith(eng, ttsProv)
	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"status": eng.Status(),
		"ms":     time.Since(start).Milliseconds(),
	})
}

// handleToolsTest 测一下 HA 是否连得上，以及能读到多少实体。
// searchTestTimeout 是"试搜"按钮的时限：比线上预算宽，用来区分
// "服务商坏了" 和 "线上设置太紧"。
const searchTestTimeout = 8 * time.Second

// handleTestSearch 用当前配置真搜一次，方便确认 key / 服务商能不能用。
func (s *server) handleTestSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	st := s.config()
	if !st.Tools.Search.Enabled {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "联网搜索未开启"})
		return
	}
	ws := tools.NewWebSearch(tools.SearchOptions{
		Provider:   st.Tools.Search.Provider,
		APIKey:     st.Tools.Search.APIKey,
		BaseURL:    st.Tools.Search.BaseURL,
		MaxResults: st.Tools.Search.MaxResults,
		// 测试给足 8s：一次 1.5s 的超时说明不了服务商坏，只能说明设置太紧。
		Timeout: searchTestTimeout,
		Always:  true,
	})
	if ws == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "还没选服务商或填 API Key"})
		return
	}
	q := strings.TrimSpace(body.Query)
	if q == "" {
		q = "今天的新闻"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	start := time.Now()
	out, err := ws.Fetch(ctx, q)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "ms": ms})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ms": ms, "query": q, "result": out})
}

// handleTestWeather 拉一次当前天气，确认 key / location 对不对。
func (s *server) handleTestWeather(w http.ResponseWriter, r *http.Request) {
	st := s.config()
	if !st.Tools.Weather.Enabled {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "天气预取未开启"})
		return
	}
	if st.Tools.Weather.Provider == "search" {
		if !st.Tools.Search.Enabled {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "天气来源选了联网搜索，但上面的搜索没开启"})
			return
		}
		ws := tools.NewWebSearch(tools.SearchOptions{
			Provider:   st.Tools.Search.Provider,
			APIKey:     st.Tools.Search.APIKey,
			BaseURL:    st.Tools.Search.BaseURL,
			MaxResults: st.Tools.Search.MaxResults,
			Timeout:    searchTestTimeout,
			Always:     true,
		})
		if ws == nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "搜索服务商或 API Key 没填"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		start := time.Now()
		// 和线上完全一致：查询词用 Weather.SearchQuery()，并把"城市对不对"一起报出来。
		wx := tools.NewWeather(tools.WeatherOptions{Provider: "search", Location: st.Tools.Weather.Location})
		q := wx.SearchQuery()
		out, err := ws.Fetch(ctx, q)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "ms": ms, "query": q})
			return
		}
		if out != "" && !wx.Relevant(out) {
			writeJSON(w, 200, map[string]any{
				"ok": false, "ms": ms, "query": q, "result": out,
				"error": "搜索结果里没有「" + wx.Location() + "」，线上会判废并退回 HA。换个更常见的城市名试试",
			})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "ms": ms, "result": out, "provider": "search", "query": q})
		return
	}

	if st.Tools.Weather.Provider != "qweather" {
		// 走 HA：直接读 weather 实体
		if st.Tools.HAURL == "" || st.Tools.HAToken == "" {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "当前使用 HA 天气，但 HA 未配置"})
			return
		}
		reg := tools.NewRegistry(tools.NewHA(st.Tools.HAURL, st.Tools.HAToken))
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		start := time.Now()
		out := reg.WeatherNow(ctx)
		if strings.TrimSpace(out) == "" {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "HA 里没有可用的 weather 实体"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "ms": time.Since(start).Milliseconds(), "result": out, "provider": "ha"})
		return
	}
	wp := tools.NewWeather(tools.WeatherOptions{
		Provider: st.Tools.Weather.Provider,
		APIKey:   st.Tools.Weather.APIKey,
		Location: st.Tools.Weather.Location,
		Days:     st.Tools.Weather.Days,
		Timeout:  time.Duration(st.Tools.Weather.TimeoutMS) * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	start := time.Now()
	out, err := wp.Current(ctx)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "ms": ms})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ms": ms, "result": out, "provider": "qweather"})
}

func (s *server) handleToolsTest(w http.ResponseWriter, r *http.Request) {
	st := s.config()
	if st.Tools.HAURL == "" || st.Tools.HAToken == "" {
		writeJSON(w, 400, map[string]any{"error": "还没填 Home Assistant 地址或令牌"})
		return
	}
	ha := tools.NewHA(st.Tools.HAURL, st.Tools.HAToken)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	states, err := ha.States(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	kinds := map[string]int{}
	for _, st := range states {
		if i := strings.IndexByte(st.EntityID, '.'); i > 0 {
			kinds[st.EntityID[:i]]++
		}
	}
	type kv struct {
		Domain string `json:"domain"`
		Count  int    `json:"count"`
	}
	list := make([]kv, 0, len(kinds))
	for k, v := range kinds {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Count > list[j].Count })
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"entities": len(states),
		"domains":  list,
		"ms":       time.Since(start).Milliseconds(),
	})
}

// handleMeta 返回后台需要的静态信息（能力、路径、可选项）。
func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	st := s.config()
	// 工具名候选要带上用户自己配的自定义工具，否则后台勾不上它们
	toolNames := tools.Names()
	s.toolsMu.RLock()
	if s.tools != nil {
		toolNames = s.tools.AllNames()
	}
	s.toolsMu.RUnlock()

	var cfgPath string
	if s.store != nil {
		cfgPath = s.store.Path()
	}
	eng := s.getEngine()
	allowSpeed := false
	if _, ok := eng.(audio.SpeedSetter); ok {
		allowSpeed = true
	}
	writeJSON(w, 200, map[string]any{
		"version":      serverVersion,
		"go":           runtime.Version(),
		"config_path":  cfgPath,
		"device_addr":  *flagDeviceAddr,
		"admin_addr":   *flagAdminAddr,
		"models":       st.Engine.ModelDir,
		"tool_names":   toolNames,
		"tools_ready":  s.tools != nil && s.tools.Ready(),
		"engine":       s.engineState(),
		"allow_speed":  allowSpeed,
		"no_engine":    *flagNoEngine,
		"audio_dir":    s.deviceOptions().AudioDir,
		"uptime_sec":   int64(time.Since(s.startedAt).Seconds()),
		"default_wake": "音箱端 keywords.txt 里的唤醒词",
		// 供应商候选直接来自 audio 注册表：后台下拉框显示的就是"这个构建真装了什么"，
		// 不会出现"选了却加载不了"的选项。
		"asr_providers": audio.ASRProviders(),
		"tts_providers": audio.TTSProviders(),
		// tts_provider_details 比 tts_providers 多三样：配置项声明（fields）、
		// 内置音色（voices）、复刻能力（clone）。后台据此把表单和音色下拉框
		// 全自动画出来——接一家新平台不需要改前端。
		"tts_provider_details": audio.TTSProviderDetails(),
		"vad_providers":        audio.VADProviders(),
		"kws_providers":        audio.KWSProviders(),
		// plugins 是「后台模块」的插件契约：前端把这里的每一项合并进导航。
		// 现在恒为空——内置页面在前端注册表里（ui/src/modules.js）。
		// 将来第三方模块只要在启动时往 modules 里注册一条
		// {id,label,group,icon,url}（url 指向它自己提供的页面），
		// 侧栏就会出现入口，不用改管理后台的代码。
		"modules": s.moduleSnapshot(),
	})
}

// handleModels 列出模型目录，方便在后台点选而不是手打路径。
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("dir")
	if root == "" {
		root = s.config().Engine.ModelDir
	}
	type item struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Kind string `json:"kind"` // file | dir
		Size int64  `json:"size,omitempty"`
	}
	out := []item{}
	entries, err := os.ReadDir(root)
	if err != nil {
		writeJSON(w, 200, map[string]any{"dir": root, "items": out, "error": err.Error()})
		return
	}
	for _, e := range entries {
		it := item{Name: e.Name(), Path: filepath.Join(root, e.Name())}
		if e.IsDir() {
			it.Kind = "dir"
		} else {
			it.Kind = "file"
			if info, err := e.Info(); err == nil {
				it.Size = info.Size()
			}
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "dir"
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, map[string]any{"dir": root, "items": out})
}

// handleRecordings 列出保存下来的录音（打开"保存录音"后才有）。
func (s *server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	dir := s.deviceOptions().AudioDir
	type item struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		At   int64  `json:"at"`
	}
	out := []item{}
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				out = append(out, item{Name: e.Name(), Size: info.Size(), At: info.ModTime().UnixMilli()})
			}
		}
	}
	// 新的在前
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	if len(out) > 100 {
		out = out[:100]
	}
	writeJSON(w, 200, map[string]any{"dir": dir, "items": out})
}

// handleRecordingFile 回放一条录音。
func (s *server) handleRecordingFile(w http.ResponseWriter, r *http.Request) {
	dir := s.deviceOptions().AudioDir
	name := r.PathValue("name")
	// 防目录穿越：只允许单层文件名
	if dir == "" || name == "" || strings.ContainsAny(name, "/\\") || name != filepath.Base(name) {
		writeJSON(w, 400, map[string]any{"error": "非法文件名"})
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, name))
}

// handleTurnsClear 清空对话记录。
func (s *server) handleTurnsClear(w http.ResponseWriter, r *http.Request) {
	s.turns.Clear()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) handleTurns(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	writeJSON(w, 200, map[string]any{"turns": s.turns.list(n)})
}

// handleStream 是 SSE：对话与状态变更实时推给后台。
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "不支持流式"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch := s.turns.subscribe()
	defer s.turns.unsubscribe(ch)

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleKWSStatus 返回唤醒词清单与各设备同步状态。
func (s *server) handleKWSStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.kwsStatus())
}

// handleKWSSync 手动触发同步；body 里的 device 为空则推给全部在线设备。
func (s *server) handleKWSSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device string `json:"device"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if s.store == nil {
		writeJSON(w, 503, map[string]any{"error": "配置未加载"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	res := s.syncKeywords(ctx, req.Device)
	out := map[string]any{"ok": true, "devices": res}
	if len(res) == 0 {
		out["note"] = "没有在线设备可同步"
	}
	writeJSON(w, 200, out)
}

// handleStats 汇总后台总览需要的数字。
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	turns := s.turns.list(200)
	var firstSum, totalSum int64
	var okCount, errCount int
	var maxFirst, minFirst int64
	for _, t := range turns {
		if t.Err != "" {
			errCount++
			continue
		}
		okCount++
		firstSum += t.FirstDeltaMS
		totalSum += t.TotalMS
		if t.FirstDeltaMS > maxFirst {
			maxFirst = t.FirstDeltaMS
		}
		if minFirst == 0 || (t.FirstDeltaMS > 0 && t.FirstDeltaMS < minFirst) {
			minFirst = t.FirstDeltaMS
		}
	}
	avg := func(sum int64, n int) int64 {
		if n == 0 {
			return 0
		}
		return sum / int64(n)
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	st := s.config()
	eng := s.getEngine()
	engineStatus := ""
	if eng != nil {
		engineStatus = eng.Status()
	}
	writeJSON(w, 200, map[string]any{
		"goroutines":     runtime.NumGoroutine(),
		"heap_mb":        int(ms.HeapAlloc / 1024 / 1024),
		"sys_mb":         int(ms.Sys / 1024 / 1024),
		"devices":        s.h.Count(),
		"turns":          len(turns),
		"ok_turns":       okCount,
		"err_turns":      errCount,
		"avg_first_ms":   avg(firstSum, okCount),
		"avg_total_ms":   avg(totalSum, okCount),
		"max_first_ms":   maxFirst,
		"min_first_ms":   minFirst,
		"audio_frames":   s.frames.Load(),
		"audio_bytes":    s.bytes.Load(),
		"audio_kbps":     s.kbps(),
		"engine":         s.engineState(),
		"engine_status":  engineStatus,
		"uptime_sec":     int64(time.Since(s.startedAt).Seconds()),
		"profiles":       len(st.Profiles),
		"tools_ready":    s.tools != nil && s.tools.Ready(),
		"tools_enabled":  len(st.Tools.Enabled),
		"keep_turns":     st.Server.KeepTurns,
		"save_audio":     st.Server.SaveAudio,
		"phone_fallback": false,
	})
}
