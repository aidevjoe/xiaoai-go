package audio

// 豆包声音复刻（Voice Clone）。
//
// 参考实现：open-xiaoai-bridge 的 scripts/clone_voice.py。它用的是
// 火山引擎 V3 复刻接口，两个端点：
//
//	POST https://openspeech.bytedance.com/api/v3/tts/voice_clone   提交训练
//	POST https://openspeech.bytedance.com/api/v3/tts/get_voice     查询状态
//
// 鉴权（旧版控制台）：
//
//	X-Api-App-Key: <AppID>
//	X-Api-Access-Key: <Access Token>
//	X-Api-Request-Id: <uuid>
//
// 新版控制台只给 API Key，改成 X-Api-Key 一个头即可（本实现两种都发，
// 服务端认哪个都行——多一个头不会报错）。
//
// 流程（和几乎所有平台的复刻一样，所以抽成了 CloneSpec 三段式）：
//
//	1. 先在控制台"买/领"一个音色槽位，拿到 S_xxxxxxxx 这样的 speaker_id；
//	2. 传一段 10~30 秒的干净人声，提交训练；
//	3. 轮询 get_voice，status 到 2（Success）或 4（Active）就能用来合成；
//	4. 合成时把 speaker_id 当音色 ID 传，并把 resource_id 换成 seed-icl-2.0。
//
// 第 4 步是最容易踩的坑：复刻音色必须配 seed-icl-2.0，
// 用 seed-tts-2.0 会报"音色不存在"。所以复刻成功后后台会提示用户切这一项，
// 而 Service 在解析音色时也会自动带出来（见 doubaoCloneResourceHint）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	doubaoCloneURL  = "https://openspeech.bytedance.com/api/v3/tts/voice_clone"
	doubaoStatusURL = "https://openspeech.bytedance.com/api/v3/tts/get_voice"
)

// doubaoCloneStatus 把平台的数字状态码翻成统一字符串。
//
//	0 NotFound / 1 Training / 2 Success / 3 Failed / 4 Active
func doubaoCloneStatus(code int) string {
	switch code {
	case 0:
		return CloneNotFound
	case 1:
		return CloneTraining
	case 2:
		return CloneReady
	case 3:
		return CloneFailed
	case 4:
		return CloneActive
	}
	return CloneNotFound
}

// doubaoClone 构造复刻能力声明。
func doubaoClone() *CloneSpec {
	return &CloneSpec{
		MinSeconds: 10,
		MaxSeconds: 30,
		Fields: []Field{
			{Key: "language", Label: "样本语种", Kind: "select", Default: "0",
				Options: []FieldOption{
					{Value: "0", Label: "中文"},
					{Value: "1", Label: "英语"},
					{Value: "2", Label: "日语"},
					{Value: "6", Label: "德语"},
					{Value: "7", Label: "法语"},
					{Value: "8", Label: "韩语"},
				}},
			{Key: "denoise", Label: "开启降噪", Kind: "bool", Default: "true",
				Help: "样本有底噪时打开；录音很干净时关掉，细节保留得更好。"},
			// 样本文字由后台面板的独立输入框提供（它同时传给 Upload 的 Text 字段，
			// 平台会用它做读音一致性校验），所以这里不再声明一个同名配置项，
			// 否则界面上会出现两个「样本文字」。
			{Key: "demo_text", Label: "试听文本（可选）", Kind: "text", Advanced: true,
				Help: "4~300 字；训练完平台会用它合成一段试听。"},
			{Key: "model_types", Label: "训练模型", Kind: "select", Default: "",
				Options: []FieldOption{
					{Value: "", Label: "平台默认"},
					{Value: "4", Label: "ICL 2.0"},
					{Value: "2", Label: "DiT 标准版（只还原音色）"},
					{Value: "3", Label: "DiT 还原版（音色 + 口音语速）"},
				},
				Advanced: true},
		},
		Upload: doubaoCloneUpload,
		Query:  doubaoCloneQuery,
		List:   doubaoCloneList,
	}
}

// doubaoCloneUpload 提交一段样本音频去训练。
func doubaoCloneUpload(ctx context.Context, cfg map[string]string, req CloneRequest) (CloneState, error) {
	if strings.TrimSpace(req.SpeakerID) == "" {
		return CloneState{}, fmt.Errorf("audio: 复刻需要 speaker_id（控制台音色槽位号，形如 S_xxxxxxxx）")
	}
	if len(req.Audio) == 0 {
		return CloneState{}, fmt.Errorf("audio: 复刻需要一段样本音频")
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "wav"
	}
	// 平台只认这几种；pcm 必须显式声明格式，其余按扩展名即可
	switch format {
	case "wav", "mp3", "ogg", "m4a", "aac", "pcm":
	default:
		return CloneState{}, fmt.Errorf("audio: 不支持的样本格式 %q（wav/mp3/ogg/m4a/aac/pcm）", format)
	}

	audio := map[string]any{
		"data":   base64.StdEncoding.EncodeToString(req.Audio),
		"format": format,
	}
	body := map[string]any{
		"speaker_id": req.SpeakerID,
		"audio":      audio,
		"language":   atoiOr(req.Params["language"], 0),
		"extra_params": map[string]any{
			"enable_audio_denoise": cfgBool(req.Params, "denoise", true),
		},
	}
	// 样本文本：给了就校验，避免"念的和写的不一样"导致训练质量差
	if t := strings.TrimSpace(firstNonEmpty(req.Text, req.Params["text"])); t != "" {
		body["text"] = t
	}
	if d := strings.TrimSpace(req.Params["demo_text"]); d != "" {
		body["extra_params"].(map[string]any)["demo_text"] = d
	}
	if mt := strings.TrimSpace(req.Params["model_types"]); mt != "" {
		body["model_types"] = []int{atoiOr(mt, 4)}
	}

	var out struct {
		BaseResp struct {
			StatusCode    int    `json:"StatusCode"`
			StatusMessage string `json:"StatusMessage"`
		} `json:"BaseResp"`
		SpeakerID              string `json:"speaker_id"`
		Status                 int    `json:"status"`
		AvailableTrainingTimes int    `json:"available_training_times"`
		SpeakerStatus          []struct {
			ModelType int    `json:"model_type"`
			DemoAudio string `json:"demo_audio"`
		} `json:"speaker_status"`
	}
	if err := doubaoClonePost(ctx, doubaoCloneURL, cfg, body, &out); err != nil {
		return CloneState{}, err
	}
	st := CloneState{
		SpeakerID: firstNonEmpty(out.SpeakerID, req.SpeakerID),
		Status:    doubaoCloneStatus(out.Status),
	}
	if out.AvailableTrainingTimes > 0 {
		st.Detail = fmt.Sprintf("剩余可训练次数：%d", out.AvailableTrainingTimes)
	}
	if out.BaseResp.StatusCode != 0 {
		st.Status = CloneFailed
		st.Detail = fmt.Sprintf("%s（code=%d）", out.BaseResp.StatusMessage, out.BaseResp.StatusCode)
	}
	st.DemoText = "你好，我是你的新声音，这样可以吗？"
	return st, nil
}

// doubaoCloneQuery 查询训练状态。
func doubaoCloneQuery(ctx context.Context, cfg map[string]string, speakerID string) (CloneState, error) {
	if strings.TrimSpace(speakerID) == "" {
		return CloneState{}, fmt.Errorf("audio: 需要 speaker_id")
	}
	var out struct {
		BaseResp struct {
			StatusCode    int    `json:"StatusCode"`
			StatusMessage string `json:"StatusMessage"`
		} `json:"BaseResp"`
		SpeakerID     string `json:"speaker_id"`
		Status        int    `json:"status"`
		SpeakerStatus []struct {
			ModelType int    `json:"model_type"`
			DemoAudio string `json:"demo_audio"`
		} `json:"speaker_status"`
	}
	if err := doubaoClonePost(ctx, doubaoStatusURL, cfg, map[string]any{"speaker_id": speakerID}, &out); err != nil {
		return CloneState{}, err
	}
	st := CloneState{SpeakerID: firstNonEmpty(out.SpeakerID, speakerID), Status: doubaoCloneStatus(out.Status)}
	for _, s := range out.SpeakerStatus {
		if s.DemoAudio != "" {
			st.DemoURL = s.DemoAudio
			break
		}
	}
	if out.BaseResp.StatusCode != 0 {
		st.Detail = out.BaseResp.StatusMessage
	}
	return st, nil
}

// doubaoCloneList 列出"能用的复刻音色"。
//
// 平台没有"列出我所有音色"的公开接口（批量查状态要另外开通 OpenAPI），
// 所以这里不做网络请求：复刻音色由用户在后台填 speaker_id 后存进配置，
// 由 Service 的本地清单一并展示（见 customVoicesFromParams）。
// 返回 nil 表示"没有额外可列的东西"，不是错误。
func doubaoCloneList(context.Context, map[string]string) ([]Voice, error) {
	return nil, nil
}

// doubaoClonePost 发一个复刻接口请求。
func doubaoClonePost(ctx context.Context, url string, cfg map[string]string, body any, out any) error {
	blob, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("audio: 构造复刻请求失败: %w", err)
	}
	timeout := time.Duration(atoiOr(cfg["timeout_ms"], 30000)) * time.Millisecond
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(blob))
	if err != nil {
		return fmt.Errorf("audio: 构造复刻请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Request-Id", randomID())
	// 新版控制台
	if k := cfgStr(cfg, "api_key", ""); k != "" {
		req.Header.Set("X-Api-Key", k)
	}
	// 旧版控制台（两个头一起发，服务端按账号体系认其中一个）
	if app := cfgStr(cfg, "app_id", ""); app != "" {
		req.Header.Set("X-Api-App-Key", app)
	}
	if ak := cfgStr(cfg, "access_key", ""); ak != "" {
		req.Header.Set("X-Api-Access-Key", ak)
	}
	if req.Header.Get("X-Api-Key") == "" && req.Header.Get("X-Api-App-Key") == "" {
		return fmt.Errorf("audio: 复刻需要 api_key，或 app_id + access_key")
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("audio: 复刻请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("audio: 读取复刻响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audio: 复刻接口返回 %d（logid=%s）: %s",
			resp.StatusCode, resp.Header.Get("X-Tt-Logid"), clipErrText(raw, 200))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("audio: 复刻返回不是合法 JSON: %w", err)
	}
	return nil
}

// customVoicesFromParams 把配置里手工登记的音色变成 Voice 条目。
//
// 两个键，语义不同但机制一样（都是"用户自己维护的一份清单"）：
//
//	voice_list   额外音色      "zh_male_xxx:低沉男声,zh_female_yyy:温柔女声"
//	clone_voices 复刻音色      "S_abc123:我的声音"
//
// 为什么要这个：平台音色太多（Edge 300+、豆包上百），内置清单只能是精选；
// 而豆包没有"列出音色"的公开接口，用户只能从控制台复制 ID。
// 有了这份清单，既可以粘贴导入，也可以把"在线拉取"的结果存下来——
// 否则拉完一刷新就没了，等于白拉。
//
// 每项支持 "ID" 或 "ID:名字"，分隔符兼容中英文冒号；逗号或换行分隔。
func customVoicesFromParams(cfg map[string]string) []Voice {
	var out []Voice
	out = append(out, parseVoiceList(cfg["voice_list"], false)...)
	out = append(out, parseVoiceList(cfg["clone_voices"], true)...)
	return out
}

func parseVoiceList(raw string, cloned bool) []Voice {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []Voice
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ';' || r == '；'
	}) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, name := item, item
		if i := strings.IndexAny(item, ":："); i > 0 {
			// 分隔符可能是全角冒号（3 字节），必须按 rune 宽度跳过
			_, sz := utf8.DecodeRuneInString(item[i:])
			id = strings.TrimSpace(item[:i])
			name = strings.TrimSpace(item[i+sz:])
		}
		if id == "" {
			continue
		}
		if name == "" {
			name = id
		}
		out = append(out, Voice{ID: id, Name: name, Cloned: cloned, Lang: "zh"})
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
