package audio

// 豆包（火山引擎 / Volcengine）语音合成。
//
// 两套接口，本实现都支持，靠 api_version 切换：
//
//	v3（默认） POST https://openspeech.bytedance.com/api/v3/tts/unidirectional
//	   头：X-Api-Key + X-Api-Resource-Id + X-Api-Request-Id
//	   体：{"req_params":{"text","speaker","audio_params":{"format","sample_rate","speech_rate"}}}
//	   返回：HTTP Chunked，一串 JSON，每个带 base64 的 data 片段
//	   —— 豆包语音合成大模型 2.0 的音色（*_bigtts，如 zh_female_vv_uranus_bigtts）
//	      只能走这条。
//
//	v1（兼容） POST https://openspeech.bytedance.com/api/v1/tts
//	   头：Authorization: Bearer;<token>
//	   体：{"app":{...},"user":{...},"audio":{"voice_type",...},"request":{...}}
//	   返回：单个 JSON，data 里是整段 base64
//	   —— 只适用于 1.0 音色（zh_female_qingxin 这类），不支持 *_bigtts。
//
// 为什么默认 v3：新版控制台只给 API Key，且 2.0 音色是当前的推荐音色；
// 老账号用 access_token + appid 的，把 api_version 改成 v1 即可。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	doubaoV3URL = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"
	doubaoV1URL = "https://openspeech.bytedance.com/api/v1/tts"
	// doubaoDefaultVoice 是豆包 2.0 的通用女声，音色自然、适配面广。
	doubaoDefaultVoice = "zh_female_vv_uranus_bigtts"
	// doubaoSessionEnd 是 v3 流式协议的"本段合成结束"码，不是错误。
	doubaoSessionEnd = 20000000
)

type doubao struct {
	version    string // v3 / v1
	apiKey     string
	appID      string
	accessKey  string
	resourceID string
	cluster    string
	voice      string
	format     string
	sampleRate int
	emotion    string
	endpoint   string
	timeout    time.Duration

	client *http.Client

	mu      sync.RWMutex
	speed   float32
	lastErr string
}

func newDoubao(opt TTSOptions) (Synthesizer, error) {
	p := opt.Params
	d := &doubao{
		version:    strings.ToLower(cfgStr(p, "api_version", "v3")),
		apiKey:     cfgStr(p, "api_key", ""),
		appID:      cfgStr(p, "app_id", ""),
		accessKey:  cfgStr(p, "access_key", ""),
		resourceID: cfgStr(p, "resource_id", "seed-tts-2.0"),
		cluster:    cfgStr(p, "cluster", "volcano_tts"),
		voice:      cfgStr(p, "voice", doubaoDefaultVoice),
		format:     strings.ToLower(cfgStr(p, "format", "pcm")),
		sampleRate: atoiOr(p["sample_rate"], 24000),
		emotion:    cfgStr(p, "emotion", ""),
		timeout:    time.Duration(atoiOr(p["timeout_ms"], 15000)) * time.Millisecond,
		speed:      opt.Speed,
	}
	if d.version != "v1" {
		d.version = "v3"
	}
	switch d.format {
	case "pcm", "wav", "mp3", "ogg_opus":
	default:
		d.format = "pcm"
	}
	// v3 的 wav 是"每个 chunk 一段 WAV"，拼起来播不了；统一改要 pcm。
	if d.version == "v3" && d.format == "wav" {
		d.format = "pcm"
	}
	d.endpoint = cfgStr(p, "endpoint", "")
	if d.speed <= 0 {
		d.speed = 1
	}

	// 鉴权方式按版本校验：新版控制台只给 API Key，旧版给 appid + access_token。
	switch d.version {
	case "v3":
		if d.apiKey == "" && (d.appID == "" || d.accessKey == "") {
			return nil, fmt.Errorf("audio: 豆包 TTS 需要 api_key（新版控制台），或 app_id + access_key（旧版控制台）")
		}
	case "v1":
		if d.appID == "" || d.accessKey == "" {
			return nil, fmt.Errorf("audio: 豆包 TTS v1 需要 app_id 与 access_key（即旧版控制台的 AppID / Access Token）")
		}
	}

	d.client = &http.Client{Timeout: d.timeout}
	return d, nil
}

// Synthesize 实现老接口（无音色覆盖）。
func (d *doubao) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return d.SynthesizeWith(ctx, SynthOptions{Text: text})
}

// SynthesizeWith 支持每次请求单独指定音色与语速。
func (d *doubao) SynthesizeWith(ctx context.Context, opt SynthOptions) ([]byte, error) {
	text := strings.TrimSpace(opt.Text)
	if text == "" {
		return nil, nil
	}
	voice := strings.TrimSpace(opt.Voice)
	if voice == "" {
		voice = d.voice
	}
	speed := opt.Speed
	if speed <= 0 {
		d.mu.RLock()
		speed = d.speed
		d.mu.RUnlock()
	}
	if speed <= 0 {
		speed = 1
	}

	reqCtx := ctx
	if d.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}

	var (
		raw []byte
		err error
	)
	if d.version == "v1" {
		raw, err = d.callV1(reqCtx, text, voice, speed)
	} else {
		raw, err = d.callV3(reqCtx, text, voice, speed)
	}
	if err != nil {
		d.setErr(err)
		return nil, err
	}
	pcm, rate, err := doubaoAudio(raw, d.format)
	if err != nil {
		d.setErr(err)
		return nil, err
	}
	d.mu.Lock()
	if rate > 0 {
		d.sampleRate = rate
	}
	d.lastErr = ""
	d.mu.Unlock()
	return pcm, nil
}

// callV3 走单向流式接口，把一串 JSON 里的音频片段拼起来。
func (d *doubao) callV3(ctx context.Context, text, voice string, speed float32) ([]byte, error) {
	body := map[string]any{
		"req_params": map[string]any{
			"text":    text,
			"speaker": voice,
			"audio_params": map[string]any{
				"format":      d.format,
				"sample_rate": d.sampleRate,
				"speech_rate": speechRateOf(speed),
			},
		},
	}
	blob, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("audio: 构造豆包请求失败: %w", err)
	}
	url := d.endpoint
	if url == "" {
		url = doubaoV3URL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("audio: 构造豆包请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-Api-Request-Id", randomID())
	req.Header.Set("X-Api-Resource-Id", d.resourceID)
	if d.apiKey != "" {
		req.Header.Set("X-Api-Key", d.apiKey)
	} else {
		// 旧版控制台：appid + access_token 两个头
		req.Header.Set("X-Api-App-Key", d.appID)
		req.Header.Set("X-Api-Access-Key", d.accessKey)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audio: 豆包 TTS 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("audio: 读取豆包响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audio: 豆包 TTS 返回 %d（logid=%s）: %s",
			resp.StatusCode, resp.Header.Get("X-Tt-Logid"), clipErrText(raw, 200))
	}
	return parseDoubaoV3(raw)
}

// callV1 走老的整段合成接口。
func (d *doubao) callV1(ctx context.Context, text, voice string, speed float32) ([]byte, error) {
	body := map[string]any{
		"app": map[string]any{
			"appid":   d.appID,
			"token":   d.accessKey,
			"cluster": d.cluster,
		},
		"user": map[string]any{"uid": "xiaoai"},
		"audio": map[string]any{
			"voice_type":   voice,
			"encoding":     d.format,
			"rate":         d.sampleRate,
			"speed_ratio":  speed,
			"volume_ratio": 1.0,
			"pitch_ratio":  1.0,
		},
		"request": map[string]any{
			"reqid":     randomID(),
			"text":      text,
			"text_type": "plain",
			"operation": "query",
		},
	}
	if d.emotion != "" {
		body["audio"].(map[string]any)["emotion"] = d.emotion
	}
	blob, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("audio: 构造豆包请求失败: %w", err)
	}
	url := d.endpoint
	if url == "" {
		url = doubaoV1URL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("audio: 构造豆包请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 注意是分号分隔，不是空格：写成 "Bearer xxx" 会 401。
	req.Header.Set("Authorization", "Bearer;"+d.accessKey)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audio: 豆包 TTS 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("audio: 读取豆包响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audio: 豆包 TTS 返回 %d: %s", resp.StatusCode, clipErrText(raw, 200))
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("audio: 豆包返回不是合法 JSON: %w", err)
	}
	if out.Code != 3000 {
		return nil, fmt.Errorf("audio: 豆包合成失败 code=%d %s", out.Code, out.Message)
	}
	b, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		return nil, fmt.Errorf("audio: 豆包音频 base64 解码失败: %w", err)
	}
	return b, nil
}

// parseDoubaoV3 从流式响应里取出全部音频。
//
// 两种形态都兼容：连写的 JSON（HTTP Chunked）与 SSE（data: {...}）。
// 官方文档只保证"每段一个 JSON、data 是 base64"，没保证换行，所以不能按行切。
func parseDoubaoV3(raw []byte) ([]byte, error) {
	var out []byte
	seen := false
	dec := json.NewDecoder(bytes.NewReader(stripSSEPrefixes(raw)))
	for {
		var msg struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			if !seen {
				return nil, fmt.Errorf("audio: 豆包返回无法解析: %w", err)
			}
			break // 已经拿到音频，尾部半截 JSON 直接忽略
		}
		seen = true
		if msg.Code != 0 && msg.Code != doubaoSessionEnd {
			return nil, fmt.Errorf("audio: 豆包合成失败 code=%d %s", msg.Code, msg.Message)
		}
		if msg.Code == doubaoSessionEnd {
			break
		}
		if msg.Data == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil {
			return nil, fmt.Errorf("audio: 豆包音频 base64 解码失败: %w", err)
		}
		out = append(out, b...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("audio: 豆包没有返回音频（检查音色 ID 与 resource_id 是否匹配）")
	}
	return out, nil
}

// stripSSEPrefixes 去掉 SSE 的 "data:" 前缀，让 json.Decoder 能吃下整段。
func stripSSEPrefixes(b []byte) []byte {
	if !bytes.Contains(b[:min(len(b), 64)], []byte("data:")) {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b))
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		line = bytes.TrimPrefix(line, []byte("data:"))
		out.Write(bytes.TrimSpace(line))
	}
	return out.Bytes()
}

// doubaoAudio 把平台返回的字节转成 PCM16/mono。
func doubaoAudio(raw []byte, format string) ([]byte, int, error) {
	switch {
	case format == "pcm":
		if len(raw) == 0 {
			return nil, 0, fmt.Errorf("audio: 豆包返回空音频")
		}
		return raw, 0, nil
	case format == "wav", isRIFF(raw):
		return decodeWAVToMono(raw)
	case format == "mp3":
		pcm, rate, err := decodeMP3ToMono(raw)
		if err != nil {
			return nil, 0, err
		}
		return pcm, rate, nil
	}
	return nil, 0, fmt.Errorf("audio: 不支持的豆包音频格式 %q（用 pcm 或 mp3）", format)
}

// TTSRate 返回当前采样率。
func (d *doubao) TTSRate() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.sampleRate > 0 {
		return d.sampleRate
	}
	return 24000
}

// SetSpeed 兼容老接口：云端 TTS 的语速其实是请求参数，这里只记兜底值。
func (d *doubao) SetSpeed(v float32) {
	if v <= 0 {
		v = 1
	}
	d.mu.Lock()
	d.speed = v
	d.mu.Unlock()
}

// Speed 返回兜底语速。
func (d *doubao) Speed() float32 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.speed > 0 {
		return d.speed
	}
	return 1
}

func (d *doubao) Status() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s := fmt.Sprintf("doubao(%s/%s)", d.version, shortID(d.voice))
	if d.lastErr != "" {
		s += ", 上次失败: " + d.lastErr
	}
	return s
}

func (d *doubao) Close() error {
	if d.client != nil {
		d.client.CloseIdleConnections()
	}
	return nil
}

func (d *doubao) setErr(err error) {
	d.mu.Lock()
	d.lastErr = clipErr(err)
	d.mu.Unlock()
}

// speechRateOf 把倍率语速换成豆包的 [-50,100] 刻度（100 = 2 倍速）。
func speechRateOf(speed float32) int {
	if speed <= 0 {
		return 0
	}
	v := int((float64(speed) - 1) * 100)
	if v > 100 {
		v = 100
	}
	if v < -50 {
		v = -50
	}
	return v
}

func shortID(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

func clipErrText(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func init() {
	RegisterTTS(TTSProvider{
		Name: "doubao",
		Desc: "豆包语音合成（火山引擎，支持声音复刻）",
		// Local=false：不需要本地模型文件，所以"没配 tts_model"也照样加载。
		Local:  false,
		Fields: doubaoFields(),
		Voices: doubaoVoices(),
		Clone:  doubaoClone(),
		New:    newDoubao,
	})
}

// doubaoFields 声明配置项。键名即 TTSParams 的键名。
func doubaoFields() []Field {
	return []Field{
		{Key: "api_version", Label: "接口版本", Kind: "select", Default: "v3",
			Options: []FieldOption{
				{Value: "v3", Label: "v3（推荐，支持 2.0 音色）"},
				{Value: "v1", Label: "v1（旧版，仅 1.0 音色）"},
			},
			Help: "新版控制台用 v3 + API Key；老账号只有 AppID/Access Token 的选 v1。"},
		{Key: "api_key", Label: "API Key", Kind: "password",
			Help: "火山引擎控制台 → API Key 管理。v3 必填（或改用 app_id + access_key）。"},
		{Key: "app_id", Label: "AppID", Kind: "text", Advanced: true,
			Help: "旧版控制台的应用标识。声音复刻也用它。"},
		{Key: "access_key", Label: "Access Token", Kind: "password", Advanced: true,
			Help: "旧版控制台的 Access Token。"},
		{Key: "resource_id", Label: "模型版本", Kind: "select", Default: "seed-tts-2.0",
			Options: []FieldOption{
				{Value: "seed-tts-2.0", Label: "seed-tts-2.0（2.0 音色）"},
				{Value: "seed-tts-1.0", Label: "seed-tts-1.0（1.0 音色）"},
				{Value: "seed-icl-2.0", Label: "seed-icl-2.0（复刻音色）"},
			},
			Help: "要和音色匹配：用 2.0 音色选 seed-tts-2.0，用复刻音色选 seed-icl-2.0。"},
		{Key: "format", Label: "音频格式", Kind: "select", Default: "pcm", Advanced: true,
			Options: []FieldOption{
				{Value: "pcm", Label: "pcm（推荐，无需解码）"},
				{Value: "mp3", Label: "mp3（更省流量）"},
			}},
		{Key: "sample_rate", Label: "采样率", Kind: "number", Default: "24000", Advanced: true},
		{Key: "cluster", Label: "集群（仅 v1）", Kind: "text", Default: "volcano_tts", Advanced: true},
		{Key: "emotion", Label: "情感（仅 v1）", Kind: "text", Advanced: true,
			Help: "部分音色支持：happy / sad / angry / surprise 等，留空用音色默认。"},
		{Key: "timeout_ms", Label: "超时（毫秒）", Kind: "number", Default: "15000", Advanced: true},
		{Key: "endpoint", Label: "接口地址覆写", Kind: "text", Advanced: true,
			Help: "走代理或自建网关时填完整 URL，留空用官方地址。"},
	}
}

// doubaoVoices 是精选音色清单。
//
// 豆包音色库有上百个（2.0 的 *_uranus_* / *_mars_* 系列、1.0 的 *_moon_* 系列……），
// 而且**没有公开的"列出音色"接口**——只能从控制台音色库复制 ID。
// 所以这里只放确认存在、覆盖面广的一批，剩下的靠后台「自定义音色清单」粘贴导入。
//
// 不要凭空补 ID：填错的音色在合成时才会报"音色不存在"，
// 比"列表短一点"糟糕得多。
func doubaoVoices() []Voice {
	return []Voice{
		{ID: "zh_female_vv_uranus_bigtts", Name: "通用女声 · 2.0", Lang: "zh", Gender: "female", Tags: []string{"推荐", "自然"}},
		{ID: "zh_female_shuangkuaisisi_moon_bigtts", Name: "爽快思思", Lang: "zh", Gender: "female", Tags: []string{"活泼"}},
		{ID: "zh_male_lengkugege_emo_v2_mars_bigtts", Name: "冷酷哥哥", Lang: "zh", Gender: "male", Tags: []string{"低沉"}},
		{ID: "zh_male_raphael_bigtts", Name: "拉斐尔 · 男声", Lang: "zh", Gender: "male"},
		{ID: "zh_female_qingxin", Name: "清新女声（1.0）", Lang: "zh", Gender: "female", Tags: []string{"1.0"}},
		{ID: "BV700_streaming", Name: "通用女声（1.0 示例）", Lang: "zh", Gender: "female", Tags: []string{"1.0"}},
	}
}
