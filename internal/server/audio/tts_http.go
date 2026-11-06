package audio

// 通用 HTTP TTS 供应商。
//
// 存在的意义是把"接一个在线语音服务"变成纯配置：不需要改服务端装配代码，
// 也不需要为每一家供应商写一个 provider——只要对方能被这样调用：
//
//	POST <url>   {"text":"要念的话","voice":"音色","speed":1.0}
//	→ 返回 WAV 或裸 PCM（16bit 单声道）
//
// 配置（后台「模型与引擎」页）：
//
//	tts_provider = "http"
//	tts_params   = {"url":"http://127.0.0.1:5002/tts","voice":"zh-CN-XiaoxiaoNeural"}
//
// 典型用法：本机跑一个 edge-tts / ChatTTS / GPT-SoVITS 的 HTTP 壳子，
// 把它的接口填进来。注意这类服务单句延迟普遍在 1~5 秒（实测 edge-tts
// 固定开销约 2 秒），所以适合"固定应答"这种能缓存的内容——固定应答缓存
// 命中后是零延迟的，只有首次会慢。整段回答走它需要自己权衡。
//
// 支持的参数（tts_params 的键）：
//
//	url         必填，接口地址
//	voice       音色名，原样放进请求体
//	format      wav（默认）/ pcm：pcm 表示响应体是裸 PCM16
//	rate        format=pcm 时的采样率，默认 24000
//	timeout_ms  超时，默认 10000
//	text_field  文本字段名，默认 text
//	voice_field 音色字段名，默认 voice
//	speed_field 语速字段名，默认 speed
//	auth        可选，原样放进 Authorization 头

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type httpTTS struct {
	url        string
	voice      string
	format     string
	rate       int
	timeout    time.Duration
	textField  string
	voiceField string
	speedField string
	auth       string

	client *http.Client

	mu    sync.RWMutex
	speed float32
	// lastErr 记录上一次失败原因，Status() 里能看见，
	// 否则"后台听不到声音"时只能翻日志。
	lastErr string
}

func newHTTPTTS(opt TTSOptions) (Synthesizer, error) {
	url := strings.TrimSpace(opt.Params["url"])
	if url == "" {
		return nil, fmt.Errorf("audio: http TTS 需要参数 url（tts_params.url）")
	}
	h := &httpTTS{
		url:        url,
		voice:      opt.Params["voice"],
		format:     strings.ToLower(defaultStr(opt.Params["format"], "wav")),
		rate:       atoiOr(opt.Params["rate"], 24000),
		timeout:    time.Duration(atoiOr(opt.Params["timeout_ms"], 10000)) * time.Millisecond,
		textField:  defaultStr(opt.Params["text_field"], "text"),
		voiceField: defaultStr(opt.Params["voice_field"], "voice"),
		speedField: defaultStr(opt.Params["speed_field"], "speed"),
		auth:       opt.Params["auth"],
		client:     &http.Client{Timeout: time.Duration(atoiOr(opt.Params["timeout_ms"], 10000)) * time.Millisecond},
		speed:      opt.Speed,
	}
	if h.speed <= 0 {
		h.speed = 1
	}
	if h.format != "pcm" {
		h.format = "wav"
	}
	return h, nil
}

// Synthesize 把文本发给 HTTP 服务，拿回 PCM。
func (h *httpTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return h.SynthesizeWith(ctx, SynthOptions{Text: text})
}

// SynthesizeWith 支持按请求指定音色：音色由「全局/档案」统一管理，
// 供应商表单里不再重复出现 voice 字段，避免两个地方都能改同一个东西。
func (h *httpTTS) SynthesizeWith(ctx context.Context, opt SynthOptions) ([]byte, error) {
	text := opt.Text
	voice := strings.TrimSpace(opt.Voice)
	if voice == "" {
		voice = h.voice
	}
	body := map[string]any{h.textField: text}
	if h.voiceField != "" && voice != "" {
		body[h.voiceField] = voice
	}
	if h.speedField != "" {
		sp := opt.Speed
		if sp <= 0 {
			h.mu.RLock()
			sp = h.speed
			h.mu.RUnlock()
		}
		if sp <= 0 {
			sp = 1
		}
		body[h.speedField] = sp
	}
	blob, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("audio: 构造 TTS 请求失败: %w", err)
	}

	reqCtx := ctx
	if h.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, h.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.url, bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("audio: 构造 TTS 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.auth != "" {
		req.Header.Set("Authorization", h.auth)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.setErr(err)
		return nil, fmt.Errorf("audio: 请求 TTS 接口失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("audio: TTS 接口返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		h.setErr(err)
		return nil, err
	}
	// 单句音频不该超过几十秒；限流防止对端返回一个巨大的流把内存吃光
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		h.setErr(err)
		return nil, fmt.Errorf("audio: 读取 TTS 响应失败: %w", err)
	}

	switch {
	case h.format == "pcm":
		if len(data) == 0 {
			return nil, fmt.Errorf("audio: TTS 接口返回空音频")
		}
		return data, nil
	case isRIFF(data):
		pcm, rate, err := decodeWAV(data)
		if err != nil {
			h.setErr(err)
			return nil, err
		}
		// 采样率以响应头为准：配置里写错也不该播出怪声
		if rate > 0 && rate != h.rate {
			h.mu.Lock()
			h.rate = rate
			h.mu.Unlock()
		}
		return pcm, nil
	default:
		// 没带 WAV 头就按配置的裸 PCM 处理（有些服务直接吐 PCM）
		if len(data) == 0 {
			return nil, fmt.Errorf("audio: TTS 接口返回空音频")
		}
		return data, nil
	}
}

func (h *httpTTS) TTSRate() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.rate > 0 {
		return h.rate
	}
	return 24000
}

func (h *httpTTS) SetSpeed(v float32) {
	if v <= 0 {
		v = 1
	}
	h.mu.Lock()
	h.speed = v
	h.mu.Unlock()
}

func (h *httpTTS) Speed() float32 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.speed
}

func (h *httpTTS) Status() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.lastErr != "" {
		return "http(" + shortHost(h.url) + ", 上次失败: " + h.lastErr + ")"
	}
	return "http(" + shortHost(h.url) + ")"
}

func (h *httpTTS) Close() error {
	if h.client != nil {
		h.client.CloseIdleConnections()
	}
	return nil
}

func (h *httpTTS) setErr(err error) {
	h.mu.Lock()
	h.lastErr = clipErr(err)
	h.mu.Unlock()
}

func init() {
	RegisterTTS(TTSProvider{
		Name: "http", Desc: "通用 HTTP 语音接口（POST 文本，返回 WAV/PCM）", Local: false,
		Fields: httpFields(),
		New:    newHTTPTTS,
	})
}

// httpFields 声明配置项。
//
// 这一家的价值在于"任何能被这样调用的服务都能接"：
//
//	POST <url>  {"text":"…","voice":"…","speed":1.0}  → WAV 或裸 PCM
//
// 所以字段名都做成可改的（text_field / voice_field / speed_field）：
// 对不上就改字段名，不用改代码。
func httpFields() []Field {
	return []Field{
		{Key: "url", Label: "接口地址", Kind: "text", Required: true,
			Default: "http://127.0.0.1:5002/tts",
			Help:    "本机跑 edge-tts / ChatTTS / GPT-SoVITS 的 HTTP 壳子时填它的地址。"},
		{Key: "format", Label: "返回格式", Kind: "select", Default: "wav",
			Options: []FieldOption{
				{Value: "wav", Label: "wav（带文件头）"},
				{Value: "pcm", Label: "pcm（裸 16bit 单声道）"},
			}},
		{Key: "rate", Label: "采样率", Kind: "number", Default: "24000", Advanced: true},
		{Key: "timeout_ms", Label: "超时（毫秒）", Kind: "number", Default: "10000", Advanced: true},
		{Key: "auth", Label: "Authorization 头", Kind: "password", Advanced: true},
		{Key: "text_field", Label: "文本字段名", Kind: "text", Default: "text", Advanced: true},
		{Key: "voice_field", Label: "音色字段名", Kind: "text", Default: "voice", Advanced: true},
		{Key: "speed_field", Label: "语速字段名", Kind: "text", Default: "speed", Advanced: true},
	}
}

// ---------- 小工具 ----------

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

func isRIFF(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE"
}

// decodeWAV 只处理最常见的 16bit PCM 单声道/立体声 WAV。
// 我们只关心"能播"，所以立体声直接取左声道降成单声道。
func decodeWAV(b []byte) ([]byte, int, error) {
	if !isRIFF(b) {
		return nil, 0, fmt.Errorf("audio: 不是 WAV 数据")
	}
	pos := 12
	rate, channels := 0, 1
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if size < 0 || body+size > len(b) {
			// 长度字段不可信时按剩余长度处理
			size = len(b) - body
		}
		switch id {
		case "fmt ":
			if size >= 16 {
				channels = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
				rate = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
			}
		case "data":
			pcm := b[body : body+size]
			if channels == 2 {
				// 双声道交错：取平均降成单声道
				n := len(pcm) / 4
				out := make([]byte, n*2)
				for i := 0; i < n; i++ {
					l := int16(binary.LittleEndian.Uint16(pcm[i*4 : i*4+2]))
					r := int16(binary.LittleEndian.Uint16(pcm[i*4+2 : i*4+4]))
					binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(int16((int(l)+int(r))/2)))
				}
				pcm = out
			}
			if rate <= 0 {
				rate = 24000
			}
			return pcm, rate, nil
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // RIFF 规定块按偶数字节对齐
		}
	}
	return nil, 0, fmt.Errorf("audio: WAV 里找不到 data 块")
}

// shortHost 从 URL 里取出 host，状态串别太长。
func shortHost(u string) string {
	s := u
	for _, p := range []string{"http://", "https://"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

func clipErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
