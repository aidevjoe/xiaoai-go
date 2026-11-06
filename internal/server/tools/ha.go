// Package tools 提供模型可调用的工具，当前是 Home Assistant 相关。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"xiaoai-go/internal/server/llm"
)

// 语音识别会听错字，这些域一旦误触发后果不可逆，直接拒绝。
var blockedDomains = map[string]bool{
	"lock": true, "alarm_control_panel": true, "valve": true,
	"script": true, "automation": true, "shell_command": true,
	"python_script": true, "homeassistant": true, "system": true,
}

// HA 是 Home Assistant 的 REST 客户端。
type HA struct {
	BaseURL string
	Token   string
	hc      *http.Client
}

// NewHA 构造 HA 客户端。
func NewHA(baseURL, token string) *HA {
	return &HA{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		hc: &http.Client{
			Timeout: 12 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}
}

// State 是 HA 实体的状态。
type State struct {
	EntityID string         `json:"entity_id"`
	State    string         `json:"state"`
	Attrs    map[string]any `json:"attributes"`
}

// FriendlyName 返回中文名。
func (s State) FriendlyName() string {
	if v, ok := s.Attrs["friendly_name"].(string); ok {
		return v
	}
	return s.EntityID
}

func (h *HA) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	if h == nil {
		return nil, fmt.Errorf("未配置 Home Assistant（启动时加 -ha-url 与 -ha-token）")
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ha: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ha: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// States 拉全部实体。
func (h *HA) States(ctx context.Context) ([]State, error) {
	b, err := h.do(ctx, http.MethodGet, "/api/states", nil)
	if err != nil {
		return nil, err
	}
	var out []State
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// State 取单个实体。
func (h *HA) State(ctx context.Context, id string) (*State, error) {
	b, err := h.do(ctx, http.MethodGet, "/api/states/"+id, nil)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// CallService 调用服务。
func (h *HA) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	_, err := h.do(ctx, http.MethodPost, fmt.Sprintf("/api/services/%s/%s", domain, service), data)
	return err
}

// Registry 把 HA 包装成模型可调用的工具集。
type Registry struct {
	// mu 保护 enabled / web / wx / wxOn / 自定义工具表。
	// 之前这些字段完全没有锁：后台保存工具开关是写 map，对话路径同时读——
	// Go 的 map 并发读写是运行时致命错误（recover 不住），线上会直接进程退出。
	mu sync.RWMutex

	ha      *HA
	enabled map[string]bool // 全局启用的工具；nil=全部启用
	custom  map[string]customTool

	// web 联网搜索、wx 天气（注入式，不注册成工具）
	web  *WebSearch
	wx   *Weather
	wxOn bool // 天气预取总开关（关掉连 HA 也不查）

	// 天气缓存：后台定时预热，问答时直接命中，不给回答路径加延迟
	wxMu   sync.Mutex
	wxText string
	wxAt   time.Time
}

// Handler 是自定义工具的执行函数。返回的字符串会原样喂给模型。
type Handler func(ctx context.Context, args map[string]any) (string, error)

type customTool struct {
	spec llm.Tool
	run  Handler
}

// Register 注册一个自定义工具（线程安全，可热插拔）。
//
// 这是给「用户自定义工具」留的口子：内置的 HA 三件套是写死的，
// 但任何外部能力（HTTP 请求、脚本、查询某个自建服务）都可以走这里注册进来，
// 由档案的 Tools 白名单决定谁能用。
func (r *Registry) Register(spec llm.Tool, run Handler) {
	if r == nil || spec.Function.Name == "" || run == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.custom == nil {
		r.custom = make(map[string]customTool)
	}
	r.custom[spec.Function.Name] = customTool{spec: spec, run: run}
}

// Unregister 移除自定义工具。
func (r *Registry) Unregister(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.custom, name)
	r.mu.Unlock()
}

// SetWeb 热替换联网搜索配置。
func (r *Registry) SetWeb(w *WebSearch) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.web = w
	r.mu.Unlock()
}

// SetWeather 热替换天气来源。enabled=false 表示彻底关掉天气预取（连 HA 也不查）。
func (r *Registry) SetWeather(w *Weather, enabled bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.wx = w
	r.wxOn = enabled
	r.mu.Unlock()
}

// NewRegistry 构造工具集。
func NewRegistry(ha *HA) *Registry {
	return &Registry{ha: ha, wxOn: true, custom: make(map[string]customTool)}
}

// Names 返回全部工具名（后台勾选用）。
func Names() []string {
	return []string{"ha_search_entities", "ha_get_state", "ha_call_service"}
}

// SetEnabled 设置全局启用的工具。names 为空表示全部启用。
//
// 这是"有些用户根本不需要工具"的落点：关掉之后模型连工具定义都看不到，
// 既省提示词也省首字延迟。
func (r *Registry) SetEnabled(names []string) {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	r.mu.Lock()
	r.enabled = m // 整体替换而不是原地改，读方拿到的永远是完整快照
	r.mu.Unlock()
}

// Enabled 判断某工具是否被全局启用。
func (r *Registry) Enabled(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.enabled) == 0 {
		return true
	}
	return r.enabled[name]
}

// Ready 表示工具集是否真的能用（配了 HA）。
func (r *Registry) Ready() bool { return r != nil && r.ha != nil }

// Allowed 按档案允许的工具名过滤。
type Filter func(name string) bool

// Specs 返回工具定义。filter 为 nil 时返回全部。
func (r *Registry) Specs(filter Filter) []llm.Tool {
	// 只暴露 2 个工具。
	// 实测：同一句话，5 个工具时首字 7.8s，2 个工具时 1.0s——
	// 工具数量对首字延迟是数量级影响。时间/天气这类能预取的，
	// 通过 Context() 直接塞进提示词，不再占工具名额。
	all := []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolSpec{
				Name:        "ha_search_entities",
				Description: "按名称关键字搜索 Home Assistant 实体，返回 entity_id、名称、状态。不确定设备名时先查，不要猜。",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"设备名关键字，如 客厅灯、书房空调"},"domain":{"type":"string","description":"可选，限定域：light/switch/climate/cover/fan/media_player"}},"required":["query"]}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolSpec{
				Name:        "ha_get_state",
				Description: "查询单个实体的详细状态。",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"entity_id":{"type":"string"}},"required":["entity_id"]}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolSpec{
				Name:        "ha_call_service",
				Description: "调用 Home Assistant 服务控制设备，例如 light.turn_on、climate.set_temperature、cover.open_cover。",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"domain":{"type":"string"},"service":{"type":"string"},"entity_id":{"type":"string","description":"目标实体，多个用逗号分隔"},"data":{"type":"object","description":"附加参数，如 {\"brightness_pct\":60}"}},"required":["domain","service","entity_id"]}`),
			},
		},
	}
	r.mu.RLock()
	for _, ct := range r.custom {
		all = append(all, ct.spec)
	}
	r.mu.RUnlock()

	out := make([]llm.Tool, 0, len(all))
	for _, t := range all {
		if !r.Enabled(t.Function.Name) {
			continue
		}
		if filter != nil && !filter(t.Function.Name) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// 天气类问法
var weatherWords = []string{"天气", "气温", "下雨", "下雪", "温度", "冷不冷", "热不热", "穿什么", "带伞"}

// 设备类问法（用来决定要不要预搜实体）
var deviceWords = []string{"灯", "空调", "风扇", "窗帘", "电视", "插座", "开关", "热水器", "扫地", "加湿", "除湿", "暖气", "地暖", "晾衣"}

// PrefetchPolicy 决定要不要预取某类信息（由助手档案提供）。
type PrefetchPolicy interface {
	Want(kind string) bool
}

// Context 预取本轮可能需要的信息，拼成一小段上下文塞进提示词。
//
// 这样"现在几点""今天天气"都变成零轮工具调用，直接回答，
// 首字延迟从 6s 降到 ~1.2s。
//
// 各项并行取：天气、设备状态、联网搜索互相不依赖，串行会把延迟加起来。
// 整体限时 prefetchBudget，谁慢就不等它（宁可少一条信息，也别让用户干等）。
func (r *Registry) Context(ctx context.Context, p PrefetchPolicy, text string) string {
	if r == nil {
		return ""
	}
	now := time.Now()
	weekday := weekdayCN(now.Weekday())

	type slot struct {
		head string
		body string
	}
	var (
		mu    sync.Mutex
		parts []slot // 按关心的顺序填充，最后统一拼接
		wg    sync.WaitGroup
	)
	wantWeather := r.wxOn && p.Want("weather") && containsAny(text, weatherWords)
	wantDevices := p.Want("devices") && r.ha != nil && containsAny(text, deviceWords)
	wantSearch := false
	query := ""
	if p.Want("web") {
		query, wantSearch = r.web.ShouldSearch(text)
	}

	// 预取最多等这么久：慢的就让它在后台继续，回答不等。
	// 之前给联网放开到 5 秒，结果"问天气"这一句每次都要多等 2~4 秒。
	pctx, cancel := context.WithTimeout(ctx, prefetchBudget)
	defer cancel()

	if wantWeather {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 缓存命中就立刻用（绝大多数情况）
			if v := r.cachedWeather(); v != "" {
				mu.Lock()
				parts = append(parts, slot{"天气实况（刚刚读取）：", v})
				mu.Unlock()
				return
			}
			// 没命中：只等 budget，等不到就让后台继续跑。
			// 关键是后台那一趟**不能被取消**——它跑完会写进缓存，
			// 下一次问天气就是 0ms，否则每次都要重新等 2~4 秒。
			ch := make(chan string, 1)
			go func() { ch <- r.fetchWeather(context.WithoutCancel(ctx)) }()
			select {
			case v := <-ch:
				if v != "" {
					r.storeWeather(v)
					mu.Lock()
					parts = append(parts, slot{"天气实况（刚刚读取）：", v})
					mu.Unlock()
				}
			case <-pctx.Done():
				// 这次不等了：回答先走，天气下次再补
			}
		}()
	}
	if wantDevices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if list, err := r.search(pctx, text, ""); err == nil && list != "" {
				mu.Lock()
				parts = append(parts, slot{"可能相关的设备（状态为刚刚实时读取，可直接使用，不要再查）：", list})
				mu.Unlock()
			}
		}()
	}
	if wantSearch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan string, 1)
			// 同样用"不可取消"的 ctx：超时只是这次不用结果，
			// 后台跑完会进 5 分钟缓存，下一句问同样的事就是 0ms。
			go func() { ch <- r.web.Search(context.WithoutCancel(ctx), query) }()
			select {
			case v := <-ch:
				if v != "" {
					mu.Lock()
					parts = append(parts, slot{
						"联网检索结果（刚刚检索，可能过时或不准确；与问题无关就直接忽略）：",
						v,
					})
					mu.Unlock()
				}
			case <-pctx.Done():
			}
		}()
	}
	wg.Wait()

	var b strings.Builder
	if p.Want("time") {
		b.WriteString("当前时间：")
		b.WriteString(now.Format("2006-01-02 15:04"))
		b.WriteString(" 星期")
		b.WriteString(weekday)
	}
	// 固定顺序输出，避免同一份信息每次顺序不同（提示词稳定，命中前缀缓存）
	for _, key := range []string{"天气实况", "可能相关的设备", "联网检索结果"} {
		for _, v := range parts {
			if !strings.HasPrefix(v.head, key) {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(v.head)
			b.WriteString(v.body)
		}
	}
	return b.String()
}

// weekdayCN 把星期几转成一个汉字。
//
// 不能直接 "一二三四五六日"[i]：Go 的字符串下标是**字节**，
// 中文一个字占 3 字节，取出来是半个字符（实测提示词里出现"星期º"，
// 模型只好自己猜今天是周几）。
func weekdayCN(d time.Weekday) string {
	// time.Weekday 里周日是 0，所以表要从"日"开始排，
	// 否则整体错一位（周四会说成周五）。
	return string([]rune("日一二三四五六")[int(d)])
}

// 预取时限：超时只是少一条信息，不能拖住回答。
// 用变量而不是常量，测试里可以调小，不必真等 3.5 秒。
var (
	prefetchBudget = 1500 * time.Millisecond // 本地/局域网信息（时间、HA、和风）
	searchBudget   = 5000 * time.Millisecond // 联网搜索（要出公网，Tavily 从国内实测 2.8~4.8s）
)

// WeatherNow 直接返回一次天气描述（后台测试用）。
func (r *Registry) WeatherNow(ctx context.Context) string {
	if r == nil {
		return ""
	}
	return r.weatherText(ctx)
}

// weatherText 取天气描述。
//
// 来源按配置选：qweather（数值准）> search（走联网检索，覆盖面最广）
// > ha（本地 weather 实体，常常只有"晴/多云"）。前一条拿不到就自动往下退，
// 免得用户问天气时听到"我没法查天气"。
func (r *Registry) weatherText(ctx context.Context) string {
	// 1) 缓存够新就直接用（后台每 10 分钟预热一次）
	r.wxMu.Lock()
	cached, at := r.wxText, r.wxAt
	r.wxMu.Unlock()
	if cached != "" && time.Since(at) < weatherTTL {
		return cached
	}
	// 2) 缓存过期/没有：现取一次，成功就更新缓存
	if v := r.fetchWeather(ctx); v != "" {
		r.wxMu.Lock()
		r.wxText, r.wxAt = v, time.Now()
		r.wxMu.Unlock()
		return v
	}
	// 3) 现取失败但有旧值：宁可给旧数据也不说"查不到"
	return cached
}

// cachedWeather 取缓存里的天气（过期返回空串）。
func (r *Registry) cachedWeather() string {
	r.wxMu.Lock()
	defer r.wxMu.Unlock()
	if r.wxText == "" || time.Since(r.wxAt) >= weatherTTL {
		return ""
	}
	return r.wxText
}

// storeWeather 写入天气缓存。
func (r *Registry) storeWeather(v string) {
	if v == "" {
		return
	}
	r.wxMu.Lock()
	r.wxText, r.wxAt = v, time.Now()
	r.wxMu.Unlock()
}

// fetchWeather 按配置真正取一次天气（qweather / search / ha 依次降级）。
func (r *Registry) fetchWeather(ctx context.Context) string {
	if r.wx != nil {
		switch r.wx.Provider() {
		case "qweather":
			if v, err := r.wx.Current(ctx); err == nil && v != "" {
				return v
			}
		case "search":
			if r.web != nil {
				if v := r.web.Search(ctx, r.wx.SearchQuery()); v != "" && r.wx.Relevant(v) {
					return v
				}
			}
		}
	}
	if r.ha == nil {
		return ""
	}
	v, err := r.weather(ctx, "")
	if err != nil {
		return ""
	}
	return humanizeHAWeather(v)
}

// weatherTTL 是天气缓存的有效期。比预热间隔长一档，
// 免得某次预热失败就立刻掉回"实时取"。
const weatherTTL = 15 * time.Minute

// ResetWeather 清掉天气缓存（配置改了就作废，否则会拿着旧城市的天气答话）。
func (r *Registry) ResetWeather() {
	if r == nil {
		return
	}
	r.wxMu.Lock()
	r.wxText, r.wxAt = "", time.Time{}
	r.wxMu.Unlock()
}

// humanizeHAWeather 把 HA 工具返回的 JSON 压成一句中文。
//
// 直接塞 JSON 进提示词模型也看得懂，但会多花几十个 token，
// 而且容易让模型照着念出字段名（"condition 是 clear-night"）。
func humanizeHAWeather(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var m struct {
		Ok          bool    `json:"ok"`
		Condition   string  `json:"condition"`
		Temperature float64 `json:"temperature"`
		Humidity    float64 `json:"humidity"`
		WindSpeed   float64 `json:"wind_speed"`
		Error       string  `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw // 解析不了就原样返回，至少不比现在差
	}
	if !m.Ok {
		// 例如 {"error":"ha: 请求失败…"}：这不是天气，注入进去模型只会道歉。
		return ""
	}
	cond := m.Condition
	if cn, ok := weatherCN[cond]; ok {
		cond = cn
	}
	out := cond
	if m.Temperature != 0 {
		out += fmt.Sprintf(" %g°C", m.Temperature)
	}
	if m.Humidity != 0 {
		out += fmt.Sprintf("，湿度 %g%%", m.Humidity)
	}
	if m.WindSpeed != 0 {
		out += fmt.Sprintf("，风速 %gm/s", m.WindSpeed)
	}
	return out
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// Exec 执行工具调用，返回 JSON 字符串。
func (r *Registry) Exec(ctx context.Context, name, argsJSON string) (string, error) {
	if !r.Enabled(name) {
		return jsonError("该工具已在后台关闭"), nil
	}
	args := map[string]any{}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return jsonError("参数不是合法 JSON"), nil
		}
	}

	switch name {
	case "get_current_time":
		return timeResult(), nil
	case "ha_get_weather":
		return r.weather(ctx, str(args["entity_id"]))
	case "ha_search_entities":
		return r.search(ctx, str(args["query"]), str(args["domain"]))
	case "ha_get_state":
		return r.getState(ctx, str(args["entity_id"]))
	case "ha_call_service":
		return r.callService(ctx, str(args["domain"]), str(args["service"]),
			str(args["entity_id"]), obj(args["data"]))
	}

	// 自定义工具（外部注册进来的）
	r.mu.RLock()
	ct, ok := r.custom[name]
	r.mu.RUnlock()
	if ok {
		out, err := ct.run(ctx, args)
		if err != nil {
			return jsonError("工具执行失败: " + err.Error()), nil
		}
		return out, nil
	}
	return jsonError("未知工具: " + name), nil
}

func timeResult() string {
	now := time.Now()
	weekday := weekdayCN(now.Weekday())
	b, _ := json.Marshal(map[string]any{
		"ok":       true,
		"datetime": now.Format("2006-01-02 15:04"),
		"weekday":  "星期" + weekday,
	})
	return string(b)
}

var weatherCN = map[string]string{
	"clear": "晴", "sunny": "晴", "partlycloudy": "多云", "cloudy": "阴",
	"rainy": "雨", "pouring": "大雨", "lightning": "雷雨",
	"snowy": "雪", "fog": "雾", "hail": "冰雹", "windy": "大风",
	"clear-night": "晴（夜间）", "partlycloudy-night": "多云（夜间）",
	"exceptional": "极端天气", "snowy-rainy": "雨夹雪",
}

func (r *Registry) weather(ctx context.Context, entityID string) (string, error) {
	var st *State
	if entityID != "" {
		s, err := r.ha.State(ctx, entityID)
		if err != nil {
			return jsonError(err.Error()), nil
		}
		st = s
	} else {
		all, err := r.ha.States(ctx)
		if err != nil {
			return jsonError(err.Error()), nil
		}
		for i := range all {
			if strings.HasPrefix(all[i].EntityID, "weather.") {
				st = &all[i]
				break
			}
		}
		if st == nil {
			return jsonError("HA 里没有 weather 实体"), nil
		}
	}
	cond := st.State
	if cn, ok := weatherCN[cond]; ok {
		cond = cn
	}
	out := map[string]any{
		"ok":          true,
		"condition":   cond,
		"temperature": st.Attrs["temperature"],
		"humidity":    st.Attrs["humidity"],
		"wind_speed":  st.Attrs["wind_speed"],
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// search 按二元组重合度 + 名词命中排序。
//
// 教训：直接用 strings.Contains 会因为实体名很啰嗦（"走廊头开关 客厅灯 开关状态切换"）
// 把真正的灯排到后面，导致"客厅的灯"搜不到"客厅筒灯1"。
func (r *Registry) search(ctx context.Context, query, domain string) (string, error) {
	all, err := r.ha.States(ctx)
	if err != nil {
		return jsonError(err.Error()), nil
	}
	q := normalizeQuery(query)
	if q == "" {
		return jsonError("请给一个设备名关键字"), nil
	}
	qGrams := grams(q)
	noun := []rune(q)
	last := ""
	if len(noun) > 0 {
		last = string(noun[len(noun)-1])
	}
	head := ""
	if len(noun) >= 2 {
		head = string(noun[:2])
	}

	type hit struct {
		score int
		s     State
	}
	var hits []hit
	for _, s := range all {
		dom := strings.SplitN(s.EntityID, ".", 2)[0]
		if domain != "" && dom != domain {
			continue
		}
		name := s.FriendlyName()
		if skipEntity(name) {
			continue
		}
		hay := grams(name)
		score := 0
		for g := range qGrams {
			if hay[g] {
				score++
			}
		}
		if last != "" && strings.Contains(name, last) {
			score += 3
		}
		if head != "" && strings.Contains(name, head) {
			score += 2
		}
		switch dom {
		case "light", "switch", "climate", "cover", "fan", "media_player":
			score++
		}
		if score > 0 {
			hits = append(hits, hit{score, s})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

	out := map[string]any{"ok": true}
	list := make([]map[string]any, 0, 6)
	for i, h := range hits {
		if i >= 6 {
			break
		}
		list = append(list, map[string]any{
			"entity_id": h.s.EntityID,
			"name":      h.s.FriendlyName(),
			"state":     h.s.State,
			"domain":    strings.SplitN(h.s.EntityID, ".", 2)[0],
		})
	}
	out["count"] = len(list)
	out["entities"] = list
	if len(list) == 0 {
		out["hint"] = "没有匹配的实体，换个关键字试试"
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func (r *Registry) getState(ctx context.Context, entityID string) (string, error) {
	if entityID == "" {
		return jsonError("缺少 entity_id"), nil
	}
	s, err := r.ha.State(ctx, entityID)
	if err != nil {
		return jsonError(err.Error()), nil
	}
	b, _ := json.Marshal(map[string]any{
		"ok": true, "entity_id": s.EntityID,
		"name": s.FriendlyName(), "state": s.State,
	})
	return string(b), nil
}

func (r *Registry) callService(ctx context.Context, domain, service, entityID string, data map[string]any) (string, error) {
	if blockedDomains[domain] {
		return jsonError(fmt.Sprintf("出于安全考虑，%s 域不允许语音控制", domain)), nil
	}
	if domain == "" || service == "" || entityID == "" {
		return jsonError("domain / service / entity_id 都不能为空"), nil
	}
	payload := map[string]any{"entity_id": entityID}
	for k, v := range data {
		payload[k] = v
	}
	if err := r.ha.CallService(ctx, domain, service, payload); err != nil {
		return jsonError(err.Error()), nil
	}
	// 回读确认真的生效，避免"嘴上说关了其实没关"
	first := strings.Split(entityID, ",")[0]
	if s, err := r.ha.State(ctx, strings.TrimSpace(first)); err == nil {
		b, _ := json.Marshal(map[string]any{"ok": true, "state": s.State})
		return string(b), nil
	}
	return `{"ok":true,"state":"unknown"}`, nil
}

// ---- 辅助 ----

var stopChars = "的了呢吗啊吧把给帮我一下这那哪个些"

func normalizeQuery(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(stopChars, r) || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(s)))
}

func grams(s string) map[string]bool {
	rs := []rune(s)
	out := make(map[string]bool, len(rs))
	if len(rs) < 2 {
		if len(rs) == 1 {
			out[string(rs)] = true
		}
		return out
	}
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = true
	}
	return out
}

func skipEntity(name string) bool {
	for _, k := range []string{"指示灯", "电量", "信号", "固件", "更新", "键盘焦点"} {
		if strings.Contains(name, k) {
			return true
		}
	}
	return false
}

func jsonError(msg string) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": msg})
	return string(b)
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
