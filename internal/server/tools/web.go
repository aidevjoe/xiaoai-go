package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// 这个文件负责「注入式」实时信息：联网搜索、天气。
//
// 为什么不做成工具调用：
//   - 语音场景没有"等模型先说要查、再等一秒结果"的余量，那样一轮要多两次往返；
//   - 这类信息（今天几号、明天天气、这条新闻）几乎每轮都要，做成工具等于每轮都多一次。
//
// 所以统一在调模型**之前**取好，拼成一小段「已知信息」塞进提示词，
// 模型直接照着说，首字延迟只多一次 HTTP 的时间（并且有缓存与超时保护）。

// SearchOptions 是联网搜索的配置。
type SearchOptions struct {
	Provider   string // bocha | tavily | serper
	APIKey     string //
	BaseURL    string // 空=用 provider 默认地址
	MaxResults int    //
	Timeout    time.Duration
	Always     bool     // true=每轮都搜（慢，只在调试时用）
	Keywords   []string // Always=false 时，命中这些词才搜
}

// WebSearch 是联网搜索客户端，带短时缓存。
type WebSearch struct {
	opt SearchOptions
	hc  *http.Client

	mu    sync.Mutex
	cache map[string]searchEntry
}

type searchEntry struct {
	text string
	at   time.Time
}

// NewWebSearch 构造搜索客户端；provider 或 key 为空时返回 nil（表示未启用）。
func NewWebSearch(opt SearchOptions) *WebSearch {
	if strings.TrimSpace(opt.Provider) == "" || strings.TrimSpace(opt.APIKey) == "" {
		return nil
	}
	if opt.MaxResults <= 0 {
		opt.MaxResults = 3
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 4500 * time.Millisecond
	}
	if len(opt.Keywords) == 0 {
		opt.Keywords = defaultSearchWords
	}
	return &WebSearch{
		opt: opt,
		// 客户端超时只是兜底：真正的时限由每次请求的 ctx 决定。
		// 之前把 http.Client.Timeout 设成配置值（默认 1.5s），
		// 而 Tavily 从国内握手就要 1 秒多，请求还没发出去就被切了。
		hc: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		cache: map[string]searchEntry{},
	}
}

// defaultSearchWords 是触发搜索的问法特征。
//
// 刻意不放「谁」「是什么」这类通用疑问词：问「你是谁」不该触发联网，
// 那既慢又容易把无关网页内容灌进提示词。
var defaultSearchWords = []string{
	"最新", "新闻", "热搜", "发布", "上市", "股价", "汇率", "比分",
	"多少钱", "价格", "报价", "官网", "搜索", "查一下", "帮我查",
	"最近发生", "今年", "今天有", "怎么回事", "靠谱吗", "推荐",
}

// ShouldSearch 判断这句话要不要联网搜索，以及实际的查询词。
func (w *WebSearch) ShouldSearch(text string) (string, bool) {
	if w == nil {
		return "", false
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	if w.opt.Always || containsAny(t, w.opt.Keywords) {
		return t, true
	}
	return "", false
}

// Search 返回格式化好的一段结果（供拼进提示词）；失败时返回空串。
func (w *WebSearch) Search(ctx context.Context, query string) string {
	if w == nil {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(query))
	w.mu.Lock()
	if e, ok := w.cache[key]; ok && time.Since(e.at) < 5*time.Minute {
		w.mu.Unlock()
		return e.text
	}
	w.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, w.opt.Timeout)
	defer cancel()
	text, err := w.search(sctx, query)
	if err != nil || strings.TrimSpace(text) == "" {
		return ""
	}
	w.mu.Lock()
	// 简单上限，避免长期运行越攒越多
	if len(w.cache) > 32 {
		w.cache = map[string]searchEntry{}
	}
	w.cache[key] = searchEntry{text: text, at: time.Now()}
	w.mu.Unlock()
	return text
}

// Fetch 直接检索一次（后台"测试"按钮用，不读缓存）。
func (w *WebSearch) Fetch(ctx context.Context, query string) (string, error) {
	if w == nil {
		return "", fmt.Errorf("联网搜索未启用")
	}
	return w.search(ctx, query)
}

type searchHit struct {
	Title   string
	URL     string
	Snippet string
}

func (w *WebSearch) search(ctx context.Context, query string) (string, error) {
	var (
		hits []searchHit
		err  error
	)
	switch strings.ToLower(w.opt.Provider) {
	case "bocha", "博查":
		hits, err = w.bocha(ctx, query)
	case "tavily":
		hits, err = w.tavily(ctx, query)
	case "serper":
		hits, err = w.serper(ctx, query)
	default:
		return "", fmt.Errorf("未知的搜索服务商: %s", w.opt.Provider)
	}
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}
	var b strings.Builder
	shown := 0
	for _, h := range hits {
		if shown >= w.opt.MaxResults {
			break
		}
		title := cleanSnippet(h.Title, 60)
		snip := cleanSnippet(h.Snippet, 110)
		// 只剩导航栏/标签墙的条目没有信息量，塞进提示词反而干扰模型
		if !looksLikeProse(title) && !looksLikeProse(snip) {
			continue
		}
		if title == "" && snip == "" {
			continue
		}
		shown++
		fmt.Fprintf(&b, "%d. %s", shown, title)
		if snip != "" {
			b.WriteString(" — ")
			b.WriteString(snip)
		}
		if host := hostOf(h.URL); host != "" {
			b.WriteString("（")
			b.WriteString(host)
			b.WriteString("）")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (w *WebSearch) bocha(ctx context.Context, query string) ([]searchHit, error) {
	base := w.opt.BaseURL
	if base == "" {
		base = "https://api.bochaai.com/v1/web-search"
	}
	body := map[string]any{"query": query, "count": w.opt.MaxResults, "summary": true}
	var out struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Summary string `json:"summary"`
					Snippet string `json:"snippet"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	if err := w.postJSON(ctx, base, body, map[string]string{"Authorization": "Bearer " + w.opt.APIKey}, &out); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, v := range out.Data.WebPages.Value {
		s := v.Summary
		if s == "" {
			s = v.Snippet
		}
		hits = append(hits, searchHit{Title: v.Name, URL: v.URL, Snippet: s})
	}
	return hits, nil
}

func (w *WebSearch) tavily(ctx context.Context, query string) ([]searchHit, error) {
	base := w.opt.BaseURL
	if base == "" {
		base = "https://api.tavily.com/search"
	}
	body := map[string]any{
		"api_key": w.opt.APIKey, "query": query,
		"max_results": w.opt.MaxResults, "search_depth": "basic",
	}
	// 新版接口改成 Bearer 认证，旧版仍认 body 里的 api_key，两个都带最稳。
	headers := map[string]string{"Authorization": "Bearer " + w.opt.APIKey}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := w.postJSON(ctx, base, body, headers, &out); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, v := range out.Results {
		hits = append(hits, searchHit{Title: v.Title, URL: v.URL, Snippet: v.Content})
	}
	return hits, nil
}

func (w *WebSearch) serper(ctx context.Context, query string) ([]searchHit, error) {
	base := w.opt.BaseURL
	if base == "" {
		base = "https://google.serper.dev/search"
	}
	body := map[string]any{"q": query, "num": w.opt.MaxResults}
	var out struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := w.postJSON(ctx, base, body, map[string]string{"X-API-KEY": w.opt.APIKey}, &out); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, v := range out.Organic {
		hits = append(hits, searchHit{Title: v.Title, URL: v.Link, Snippet: v.Snippet})
	}
	return hits, nil
}

func (w *WebSearch) postJSON(ctx context.Context, url string, body any, headers map[string]string, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return w.do(req, out)
}

func (w *WebSearch) do(req *http.Request, out any) error {
	resp, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, clipText(string(data), 120))
	}
	return json.Unmarshal(data, out)
}

// WeatherOptions 是天气来源配置。
type WeatherOptions struct {
	Provider string // ha | qweather | search
	APIKey   string
	Location string // 和风：LocationID（如 101010100）或 "经度,纬度"
	Days     int    // 预报天数，0=只要实况
	Timeout  time.Duration
	BaseURL  string // 空=和风官方域名；走代理或自建网关时填
}

// Provider 返回天气来源（ha / qweather / search）。
func (w *Weather) Provider() string {
	if w == nil {
		return ""
	}
	return w.opt.Provider
}

// SearchQuery 返回走联网搜索时用的查询词。
//
// 实测（Tavily）："杭州 今天天气" 会搜出**惠州**的天气——
// 空格加"今天"让检索跑偏；"杭州天气" 才是对的。所以这里只用「城市+天气」。
func (w *Weather) SearchQuery() string {
	if w == nil {
		return ""
	}
	loc := strings.TrimSpace(w.opt.Location)
	if loc == "" {
		return "今天天气"
	}
	return loc + "天气"
}

// Location 返回配置里的城市。
func (w *Weather) Location() string {
	if w == nil {
		return ""
	}
	return strings.TrimSpace(w.opt.Location)
}

// Relevant 判断搜索结果是不是真的在说这个城市。
//
// 搜索对中文地名不总是可靠（实测把杭州搜成了惠州），
// 结果里连城市名都没有就直接判废——宁可退回 HA，也别答错城市。
func (w *Weather) Relevant(text string) bool {
	loc := w.Location()
	if loc == "" || text == "" {
		return text != ""
	}
	// 只有纯汉字地名才做这个校验（英文名/经纬度不适用）
	han := 0
	for _, r := range loc {
		if unicode.Is(unicode.Han, r) {
			han++
		}
	}
	if han < 2 {
		return true
	}
	return strings.Contains(text, loc)
}

// Weather 拉取天气实况。
//
// 默认走 HA 的 weather 实体，但它来自本地传感器/气象集成，常常只更新到"晴/多云"
// 这种粗略状态，温度和预报经常不准。所以支持直接接和风天气（国内数据、免费额度够用），
// 让模型拿到的是真实数值。
type Weather struct {
	opt WeatherOptions
	hc  *http.Client

	mu  sync.Mutex
	cur string
	at  time.Time
}

// NewWeather 构造天气客户端；provider=qweather 时必须有 key 和 location。
func NewWeather(opt WeatherOptions) *Weather {
	if opt.Timeout <= 0 {
		opt.Timeout = 4500 * time.Millisecond
	}
	if opt.Provider == "" {
		opt.Provider = "ha"
	}
	return &Weather{
		opt: opt,
		hc: &http.Client{
			Timeout: opt.Timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}
}

// Ready 判断该来源是否可用。
func (w *Weather) Ready() bool {
	if w == nil {
		return false
	}
	if w.opt.Provider == "qweather" {
		return w.opt.APIKey != "" && w.opt.Location != ""
	}
	return false
}

// Current 返回一句中文天气描述（带缓存，10 分钟内不重复请求）。
func (w *Weather) Current(ctx context.Context) (string, error) {
	if !w.Ready() {
		return "", fmt.Errorf("天气来源未配置")
	}
	w.mu.Lock()
	if w.cur != "" && time.Since(w.at) < 10*time.Minute {
		v := w.cur
		w.mu.Unlock()
		return v, nil
	}
	w.mu.Unlock()

	v, err := w.fetch(ctx)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	w.cur, w.at = v, time.Now()
	w.mu.Unlock()
	return v, nil
}

func (w *Weather) fetch(ctx context.Context) (string, error) {
	base := strings.TrimRight(w.opt.BaseURL, "/")
	if base == "" {
		base = "https://devapi.qweather.com/v7/weather"
	}
	var now struct {
		Code string `json:"code"`
		Now  struct {
			Temp      string `json:"temp"`
			FeelsLike string `json:"feelsLike"`
			Text      string `json:"text"`
			WindDir   string `json:"windDir"`
			WindScale string `json:"windScale"`
			Humidity  string `json:"humidity"`
			ObsTime   string `json:"obsTime"`
		} `json:"now"`
	}
	if err := w.getJSON(ctx, fmt.Sprintf("%s/now?location=%s&key=%s", base, w.opt.Location, w.opt.APIKey), &now); err != nil {
		return "", err
	}
	if now.Code != "200" {
		return "", fmt.Errorf("和风天气返回 code=%s", now.Code)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s°C（体感 %s°C），湿度 %s%%，%s %s级",
		now.Now.Text, now.Now.Temp, now.Now.FeelsLike,
		now.Now.Humidity, now.Now.WindDir, now.Now.WindScale)

	if w.opt.Days > 0 {
		var fc struct {
			Code  string `json:"code"`
			Daily []struct {
				FxDate    string `json:"fxDate"`
				TempMax   string `json:"tempMax"`
				TempMin   string `json:"tempMin"`
				TextDay   string `json:"textDay"`
				TextNight string `json:"textNight"`
			} `json:"daily"`
		}
		days := w.opt.Days
		if days > 7 {
			days = 7
		}
		u := fmt.Sprintf("%s/%dd?location=%s&key=%s", base, days, w.opt.Location, w.opt.APIKey)
		if err := w.getJSON(ctx, u, &fc); err == nil && fc.Code == "200" && len(fc.Daily) > 0 {
			b.WriteString("；未来预报：")
			for i, d := range fc.Daily {
				if i > 0 {
					b.WriteString("；")
				}
				fmt.Fprintf(&b, "%s %s %s~%s°C", shortDate(d.FxDate), d.TextDay, d.TempMin, d.TempMax)
			}
		}
	}
	return b.String(), nil
}

func (w *Weather) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, clipText(string(data), 120))
	}
	return json.Unmarshal(data, out)
}

// shortDate 把 2026-11-06 变成 11-06。
func shortDate(s string) string {
	if len(s) >= 10 {
		return s[5:10]
	}
	return s
}

// hostOf 取 URL 的域名，用于标注信息来源。
func hostOf(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// imageTag 匹配搜索结果里的 "Image 47:" 这类图片占位标记。
var imageTag = regexp.MustCompile(`Image\s*\d+:?`)

// cleanSnippet 把网页摘要压成一行：去图片标记、折叠所有空白、按需截断。
//
// 搜索引擎返回的正文常常带一大段导航（"首页 预报 预警 雷达 云图 …"），
// 原样塞进提示词既占 token 又会让模型抓错重点。
func cleanSnippet(s string, max int) string {
	s = imageTag.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	return clipText(s, max)
}

// looksLikeProse 粗略判断一段文本是不是"有内容的句子"：
// 汉字/字母占比够高就算。导航墙通常是大量空格分隔的短词，占比会偏低。
func looksLikeProse(s string) bool {
	if s == "" {
		return false
	}
	n, word := 0, 0
	for _, r := range s {
		n++
		if unicode.Is(unicode.Han, r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
			word++
		}
	}
	return n > 0 && word*100/n >= 65
}

// clipText 按字符截断，避免把整段网页塞进提示词。
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
