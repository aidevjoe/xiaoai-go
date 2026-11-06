package audio

// Edge TTS：直连微软 Edge 浏览器"大声朗读"背后的语音服务。
//
// 参考实现：difyz9/edge-tts-go（Python edge-tts 的 Go 移植）。协议要点：
//
//	1. 建 WebSocket，URL 上带 DRM 令牌：
//	     wss://api.msedgeservices.com/tts/cognitiveservices/websocket/v1
//	       ?Ocp-Apim-Subscription-Key=<TrustedClientToken>
//	       &Sec-MS-GEC=<按 5 分钟窗口哈希出来的令牌>
//	       &Sec-MS-GEC-Version=1-<Chromium 版本>
//	       &ConnectionId=<uuid>
//	2. 发 speech.config（告诉服务端要什么格式）
//	3. 发一段 SSML（Path:ssml）
//	4. 收帧：文本帧是元数据（turn.start/end），二进制帧 = 2 字节头长 + 头 + 音频
//
// Sec-MS-GEC 的算法（这是唯一"有门槛"的地方）：
//
//	t = 当前 UTC 秒 → 向下取整到 300 秒 → 加上 1601 年以来的秒数
//	→ 乘 10^7 得到 Windows FILETIME ticks → 拼上 TrustedClientToken
//	→ SHA-256 → 十六进制大写
//
// 令牌每 5 分钟换一次，所以服务端时钟必须准（NAS 一般是 NTP 同步的）。
//
// 输出格式默认要 mp3（服务端对 mp3 支持最稳），本地用 go-mp3 解成 PCM。
// 也可以选 pcm，省掉解码——如果某次 Edge 改了支持范围，切回 mp3 即可。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	edgeTrustedToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeWSSBase      = "wss://api.msedgeservices.com/tts/cognitiveservices/websocket/v1"
	// 音色清单只有老域名还开放（实测 2026-11：新版 api.msedgeservices.com
	// 的 voices/list 直接 403）。合成仍走新域名的 WebSocket。
	edgeVoiceListURL = "https://speech.platform.bing.com/consumer/speech/synthesize/readaloud/voices/list"
	// edgeChromiumVersion 会同时出现在 UA 与 Sec-MS-GEC-Version 里，
	// 两边必须一致，否则服务端会以 403 拒绝握手。
	edgeChromiumVersion = "140.0.3485.14"
	edgeChromiumMajor   = "140"
	edgeDefaultVoice    = "zh-CN-XiaoxiaoNeural"
	// winEpochSeconds 是 1601-01-01 到 1970-01-01 的秒数。
	winEpochSeconds = 11644473600
)

type edgeTTS struct {
	voice    string
	format   string // mp3 / pcm / wav
	rate     int    // 请求的采样率（pcm/wav 用）
	proxy    string // 预留：走代理时填 host:port
	timeout  time.Duration
	endpoint string

	mu      sync.RWMutex
	speed   float32
	lastErr string
}

func newEdgeTTS(opt TTSOptions) (Synthesizer, error) {
	p := opt.Params
	e := &edgeTTS{
		voice:    cfgStr(p, "voice", edgeDefaultVoice),
		format:   strings.ToLower(cfgStr(p, "format", "mp3")),
		rate:     atoiOr(p["sample_rate"], 24000),
		proxy:    cfgStr(p, "proxy", ""),
		timeout:  time.Duration(atoiOr(p["timeout_ms"], 20000)) * time.Millisecond,
		endpoint: cfgStr(p, "endpoint", ""),
		speed:    opt.Speed,
	}
	switch e.format {
	case "mp3", "pcm", "wav":
	default:
		e.format = "mp3"
	}
	if e.speed <= 0 {
		e.speed = 1
	}
	return e, nil
}

// edgeOutputFormat 把内部格式名翻成 Edge 的 outputFormat。
func (e *edgeTTS) edgeOutputFormat() string {
	switch e.format {
	case "pcm":
		return "raw-24khz-16bit-mono-pcm"
	case "wav":
		return "riff-24khz-16bit-mono-pcm"
	}
	return "audio-24khz-48kbitrate-mono-mp3"
}

// Synthesize 实现老接口。
func (e *edgeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return e.SynthesizeWith(ctx, SynthOptions{Text: text})
}

// SynthesizeWith 合成一句话：一次握手、一段 SSML、收完即走。
func (e *edgeTTS) SynthesizeWith(ctx context.Context, opt SynthOptions) ([]byte, error) {
	text := strings.TrimSpace(opt.Text)
	if text == "" {
		return nil, nil
	}
	voice := strings.TrimSpace(opt.Voice)
	if voice == "" {
		voice = e.voice
	}
	speed := opt.Speed
	if speed <= 0 {
		e.mu.RLock()
		speed = e.speed
		e.mu.RUnlock()
	}
	if speed <= 0 {
		speed = 1
	}

	reqCtx := ctx
	if e.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	raw, err := e.stream(reqCtx, text, voice, speed)
	if err != nil {
		e.setErr(err)
		return nil, err
	}
	pcm, rate, err := edgeAudio(raw, e.format)
	if err != nil {
		e.setErr(err)
		return nil, err
	}
	e.mu.Lock()
	e.lastErr = ""
	if rate > 0 {
		e.rate = rate
	}
	e.mu.Unlock()
	return pcm, nil
}

// stream 完成"连接 → config → ssml → 收音频"的全过程。
func (e *edgeTTS) stream(ctx context.Context, text, voice string, speed float32) ([]byte, error) {
	connID := strings.ReplaceAll(randomID(), "-", "")
	url := e.wsURL(connID)

	dialCtx := ctx
	var cancel context.CancelFunc
	if e.timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"User-Agent":             {edgeUserAgent()},
			"Origin":                 {"chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"},
			"Pragma":                 {"no-cache"},
			"Cache-Control":          {"no-cache"},
			"Accept-Encoding":        {"gzip, deflate, br"},
			"Accept-Language":        {"en-US,en;q=0.9"},
			"Sec-WebSocket-Protocol": {"synthesize"},
		},
	})
	if err != nil {
		// 429 是微软对出口 IP 的限流（实测同一 IP 连续请求会中招），
		// 不是配置错误；把它翻译成人话，否则用户会去翻密钥。
		if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("audio: Edge 限流了（429），这是免费服务的出口 IP 限制，等几秒再试或换一个平台")
		}
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("audio: Edge 拒绝握手（403），多半是本机时钟不准（Sec-MS-GEC 依赖时间）")
		}
		return nil, fmt.Errorf("audio: 连接 Edge TTS 失败: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	// 单句音频最多几十秒，给足读超时即可
	conn.SetReadLimit(8 << 20)

	now := time.Now().UTC()
	ts := now.Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")

	// 1) speech.config：告诉服务端输出格式与"不要字幕"
	cfg := fmt.Sprintf(
		"X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n"+
			`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"%s"}}}}`,
		ts, e.edgeOutputFormat())
	if err := conn.Write(ctx, websocket.MessageText, []byte(cfg)); err != nil {
		return nil, fmt.Errorf("audio: 发送 Edge 配置失败: %w", err)
	}

	// 2) SSML：音色 + 语速都在这里
	ssml := fmt.Sprintf(
		"X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%sZ\r\nPath:ssml\r\n\r\n"+
			"<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='%s'>"+
			"<voice name='%s'><prosody pitch='+0Hz' rate='%s' volume='+0%%'>%s</prosody></voice></speak>",
		connID, now.Format("2006-01-02T15:04:05.000"), ssmlLang(voice), xmlEscape(voice),
		edgeRate(speed), xmlEscape(sanitizeForSSML(text)))
	if err := conn.Write(ctx, websocket.MessageText, []byte(ssml)); err != nil {
		return nil, fmt.Errorf("audio: 发送 Edge SSML 失败: %w", err)
	}

	// 3) 收帧直到 turn.end
	var audio bytes.Buffer
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if audio.Len() > 0 {
				break // 已经拿到音频，连接被对端关掉也无所谓
			}
			return nil, fmt.Errorf("audio: 读取 Edge 响应失败: %w", err)
		}
		if typ == websocket.MessageBinary {
			if payload, ok := edgeBinaryPayload(data); ok {
				audio.Write(payload)
			}
			continue
		}
		// 文本帧：看 Path 判断是不是结束
		if bytes.Contains(data, []byte("Path:turn.end")) {
			break
		}
		if bytes.Contains(data, []byte("Path:turn.start")) {
			continue
		}
	}
	if audio.Len() == 0 {
		return nil, fmt.Errorf("audio: Edge 没有返回音频（音色 %q 可能不存在）", voice)
	}
	return audio.Bytes(), nil
}

// wsURL 拼出带 DRM 令牌的连接地址。
func (e *edgeTTS) wsURL(connID string) string {
	base := e.endpoint
	if base == "" {
		base = edgeWSSBase
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep +
		"Ocp-Apim-Subscription-Key=" + edgeTrustedToken +
		"&Sec-MS-GEC=" + edgeSecMSGEC(time.Now()) +
		"&Sec-MS-GEC-Version=1-" + edgeChromiumVersion +
		"&ConnectionId=" + connID
}

// edgeSecMSGEC 算出当前 5 分钟窗口的 DRM 令牌。
func edgeSecMSGEC(now time.Time) string {
	// 向下取整到 5 分钟（300 秒）：令牌在同一个窗口内保持不变
	sec := now.UTC().Unix()
	sec -= sec % 300
	ticks := (sec + winEpochSeconds) * 10_000_000
	sum := sha256.Sum256([]byte(strconv.FormatInt(ticks, 10) + edgeTrustedToken))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// edgeBinaryPayload 从二进制帧里取出音频。
//
// 帧结构：2 字节大端头长度 + 头（文本）+ 音频。头里带 Path:audio。
func edgeBinaryPayload(b []byte) ([]byte, bool) {
	if len(b) < 2 {
		return nil, false
	}
	hlen := int(binary.BigEndian.Uint16(b[:2]))
	if hlen <= 0 || 2+hlen > len(b) {
		return nil, false
	}
	header := b[2 : 2+hlen]
	if !bytes.Contains(header, []byte("Path:audio")) {
		return nil, false
	}
	return b[2+hlen:], true
}

// edgeAudio 把 Edge 返回的字节转成 PCM16/mono。
func edgeAudio(raw []byte, format string) ([]byte, int, error) {
	switch format {
	case "pcm":
		if len(raw) == 0 {
			return nil, 0, fmt.Errorf("audio: Edge 返回空音频")
		}
		return raw, 0, nil
	case "wav":
		return decodeWAVToMono(raw)
	}
	pcm, rate, err := decodeMP3ToMono(raw)
	if err != nil {
		return nil, 0, err
	}
	return pcm, rate, nil
}

// Discover 拉取 Edge 的完整音色清单（官方开放接口，无需鉴权，只要 TrustedClientToken）。
func edgeDiscover(ctx context.Context, _ map[string]string) ([]Voice, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		edgeVoiceListURL+"?trustedclienttoken="+edgeTrustedToken, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", edgeUserAgent())
	req.Header.Set("Accept", "*/*")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("audio: 拉取 Edge 音色列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audio: Edge 音色列表返回 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var list []struct {
		ShortName    string `json:"ShortName"`
		Locale       string `json:"Locale"`
		Gender       string `json:"Gender"`
		FriendlyName string `json:"FriendlyName"`
		VoiceTag     struct {
			VoicePersonalities []string `json:"VoicePersonalities"`
		} `json:"VoiceTag"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("audio: Edge 音色列表解析失败: %w", err)
	}
	out := make([]Voice, 0, len(list))
	for _, v := range list {
		if v.ShortName == "" {
			continue
		}
		g := strings.ToLower(v.Gender)
		if g != "female" && g != "male" {
			g = ""
		}
		out = append(out, Voice{
			ID:     v.ShortName,
			Name:   edgeVoiceName(v.FriendlyName, v.ShortName),
			Lang:   v.Locale,
			Gender: g,
			Tags:   v.VoiceTag.VoicePersonalities,
		})
	}
	return out, nil
}

// edgeVoiceName 把 "Microsoft Xiaoxiao Online (Natural) - Chinese (Mainland)"
// 压成 "Xiaoxiao Online (Chinese (Mainland))" —— 后台下拉框没那么宽。
func edgeVoiceName(friendly, short string) string {
	f := strings.TrimPrefix(friendly, "Microsoft ")
	f = strings.ReplaceAll(f, " - ", " · ")
	if f == "" {
		return short
	}
	return f
}

func edgeUserAgent() string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36" +
		" (KHTML, like Gecko) Chrome/" + edgeChromiumMajor + ".0.0.0 Safari/537.36" +
		" Edg/" + edgeChromiumMajor + ".0.0.0"
}

// ssmlLang 由音色名推出 xml:lang：zh-CN-XiaoxiaoNeural → zh-CN。
func ssmlLang(voice string) string {
	parts := strings.SplitN(voice, "-", 3)
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return "zh-CN"
}

// edgeRate 把倍率换成 SSML 的百分比：1.0 → +0%，1.2 → +20%。
func edgeRate(speed float32) string {
	pct := int((float64(speed) - 1) * 100)
	if pct > 100 {
		pct = 100
	}
	if pct < -50 {
		pct = -50
	}
	if pct >= 0 {
		return "+" + strconv.Itoa(pct) + "%"
	}
	return strconv.Itoa(pct) + "%"
}

// sanitizeForSSML 去掉 SSML 里会致命的东西（控制字符与未转义的裸 &）。
func sanitizeForSSML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteRune(' ')
			continue
		}
		if r < 0x20 {
			// 控制字符服务端会直接拒收；换成空格而不是删掉，
			// 免得不小心把两个词粘成一个（"a\x00b" → "ab"）。
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// xmlEscape 转义 XML 特殊字符（Edge 会直接拒收不合法 SSML）。
func xmlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\'':
			b.WriteString("&apos;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (e *edgeTTS) TTSRate() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.rate > 0 {
		return e.rate
	}
	return 24000
}

func (e *edgeTTS) SetSpeed(v float32) {
	if v <= 0 {
		v = 1
	}
	e.mu.Lock()
	e.speed = v
	e.mu.Unlock()
}

func (e *edgeTTS) Speed() float32 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.speed > 0 {
		return e.speed
	}
	return 1
}

func (e *edgeTTS) Status() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := "edge(" + shortID(e.voice) + "/" + e.format + ")"
	if e.lastErr != "" {
		s += ", 上次失败: " + e.lastErr
	}
	return s
}

func (e *edgeTTS) Close() error { return nil }

func (e *edgeTTS) setErr(err error) {
	e.mu.Lock()
	e.lastErr = clipErr(err)
	e.mu.Unlock()
}

func init() {
	RegisterTTS(TTSProvider{
		Name:     "edge",
		Desc:     "微软 Edge 在线语音（免费、音色多、无需密钥）",
		Local:    false,
		Fields:   edgeFields(),
		Voices:   edgeVoices(),
		Discover: edgeDiscover,
		New:      newEdgeTTS,
	})
}

func edgeFields() []Field {
	return []Field{
		{Key: "format", Label: "音频格式", Kind: "select", Default: "mp3",
			Options: []FieldOption{
				{Value: "mp3", Label: "mp3（最稳，本地解码）"},
				{Value: "pcm", Label: "pcm（免解码，若报错切回 mp3）"},
			}},
		{Key: "sample_rate", Label: "采样率", Kind: "number", Default: "24000", Advanced: true},
		{Key: "timeout_ms", Label: "超时（毫秒）", Kind: "number", Default: "20000", Advanced: true},
		{Key: "endpoint", Label: "接口地址覆写", Kind: "text", Advanced: true,
			Help: "微软换域名时不用改代码，填新的 wss:// 地址即可。"},
	}
}

// edgeVoices 是常用中文音色精选；完整清单靠 Discover 在线拉。
func edgeVoices() []Voice {
	return []Voice{
		{ID: "zh-CN-XiaoxiaoNeural", Name: "晓晓 · 温柔女声", Lang: "zh-CN", Gender: "female", Tags: []string{"推荐", "自然"}},
		{ID: "zh-CN-XiaoyiNeural", Name: "晓伊 · 活泼女声", Lang: "zh-CN", Gender: "female", Tags: []string{"活泼"}},
		{ID: "zh-CN-YunxiNeural", Name: "云希 · 阳光男声", Lang: "zh-CN", Gender: "male", Tags: []string{"推荐"}},
		{ID: "zh-CN-YunjianNeural", Name: "云健 · 浑厚男声", Lang: "zh-CN", Gender: "male"},
		{ID: "zh-CN-YunyangNeural", Name: "云扬 · 播音男声", Lang: "zh-CN", Gender: "male", Tags: []string{"新闻"}},
		{ID: "zh-CN-liaoning-XiaobeiNeural", Name: "晓北 · 东北话", Lang: "zh-CN", Gender: "female", Tags: []string{"方言"}},
		{ID: "zh-CN-shaanxi-XiaoniNeural", Name: "晓妮 · 陕西话", Lang: "zh-CN", Gender: "female", Tags: []string{"方言"}},
		{ID: "zh-HK-HiuGaaiNeural", Name: "曉佳 · 粤语", Lang: "zh-HK", Gender: "female", Tags: []string{"粤语"}},
		{ID: "zh-TW-HsiaoChenNeural", Name: "曉臻 · 台湾国语", Lang: "zh-TW", Gender: "female"},
		{ID: "en-US-AriaNeural", Name: "Aria · 美式女声", Lang: "en-US", Gender: "female"},
		{ID: "en-US-GuyNeural", Name: "Guy · 美式男声", Lang: "en-US", Gender: "male"},
		{ID: "ja-JP-NanamiNeural", Name: "七海 · 日语", Lang: "ja-JP", Gender: "female"},
	}
}
