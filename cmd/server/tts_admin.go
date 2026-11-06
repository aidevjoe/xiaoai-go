package main

// TTS 管理接口。
//
// 全部围绕"通用接入层"设计：这里不认识豆包也不认识 Edge，
// 只把 audio 包里供应商自己声明的东西（配置项、音色、复刻能力）转发给后台。
// 因此新增一家平台不需要改这个文件。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xiaoai-go/internal/server/audio"
)

// ttsPreviewTimeout 是试听的时限：云端 TTS 正常 300~800ms，
// 但首次握手 + 大音色可能要 2~3 秒，给足余量才能区分"慢"和"坏"。
const ttsPreviewTimeout = 20 * time.Second

// handleTTSProviders 返回全部供应商的完整声明。
//
// meta 里已经带了一份，这个接口用于"打开面板时刷新"——
// 例如插件在运行期注册了新供应商。
func (s *server) handleTTSProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"providers": audio.TTSProviderDetails()})
}

// handleTTSVoices 列出某供应商的音色（内置清单 + 复刻音色 + 在线拉取）。
//
// 参数从"已保存的配置"里取，不从请求里取：凭据不该出现在 URL 上，
// 也不该让前端拿着明文密钥来回传。
func (s *server) handleTTSVoices(w http.ResponseWriter, r *http.Request) {
	provider := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider")))
	if provider == "" {
		writeJSON(w, 400, map[string]any{"error": "需要 provider 参数"})
		return
	}
	st := s.config()
	cfg := st.Engine.TTSProviders[provider]
	// refresh=1 表示用户点了「刷新音色」：绕过缓存重新拉
	refresh := r.URL.Query().Get("refresh") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	voices, err := s.speech.Voices(ctx, provider, cfg, refresh)
	out := map[string]any{"provider": provider, "voices": voices}
	if err != nil {
		// 拉取失败不影响内置清单：把错误原样带出去，让后台提示"在线清单没拿到"
		out["error"] = err.Error()
	}
	writeJSON(w, 200, out)
}

// handleTTSPreview 合成一句话给人耳验证，返回 WAV。
//
// 支持两种用法：
//   - 不带参数：用当前生效的全局配置
//   - 带 provider/voice/params：试听"还没保存"的组合（后台改完先听再存）
func (s *server) handleTTSPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text     string            `json:"text"`
		Provider string            `json:"provider"`
		Voice    string            `json:"voice"`
		Speed    float32           `json:"speed"`
		Params   map[string]string `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		text = "你好，我是你的语音助手。"
	}
	if s.speech == nil {
		writeJSON(w, 503, map[string]any{"error": "语音服务未初始化"})
		return
	}

	st := s.config()
	choice := audio.VoiceChoice{
		Provider: req.Provider,
		Voice:    req.Voice,
		Speed:    req.Speed,
	}
	// 参数以"已保存的配置"打底，请求里的覆盖——这样试听用的就是线上的真实凭据，
	// 前端只需要传"想换的那一项"。
	if req.Provider != "" {
		base := st.Engine.TTSProviders[strings.ToLower(req.Provider)]
		if len(base) > 0 || len(req.Params) > 0 {
			merged := make(map[string]string, len(base)+len(req.Params))
			for k, v := range base {
				merged[k] = v
			}
			for k, v := range req.Params {
				if strings.TrimSpace(v) != "" {
					merged[k] = v
				}
			}
			choice.Params = merged
		}
	}
	if choice.Speed <= 0 {
		choice.Speed = st.Voice.Speed
	}

	ctx, cancel := context.WithTimeout(r.Context(), ttsPreviewTimeout)
	defer cancel()
	start := time.Now()
	pcm, rate, err := s.speech.Synthesize(ctx, text, choice)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error(), "ms": ms})
		return
	}
	// 有些平台在音色 ID 写错时返回 200 + 静音，光看状态码发现不了。
	// 这里顺带判一下，让后台能提示"合成了但没声音"。
	if !audio.HasAudio(pcm) {
		writeJSON(w, 200, map[string]any{
			"error": "合成成功但音频是静音，检查音色 ID 与模型版本（resource_id）是否匹配",
			"ms":    ms, "bytes": len(pcm), "rate": rate,
		})
		return
	}
	w.Header().Set("X-TTS-Ms", strconv.FormatInt(ms, 10))
	w.Header().Set("X-TTS-Rate", strconv.Itoa(rate))
	writeWAV(w, pcm, rate)
}

// handleTTSClone 提交一次声音复刻。
//
// 音频用 base64 传（浏览器里拿到的就是 File → ArrayBuffer），
// 限制 10MB 与平台侧一致：超过这个大小平台也会拒。
func (s *server) handleTTSClone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider  string            `json:"provider"`
		SpeakerID string            `json:"speaker_id"`
		Format    string            `json:"format"`
		Text      string            `json:"text"`
		Audio     string            `json:"audio"` // base64
		Params    map[string]string `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 14<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体不是合法 JSON（音频过大？上限 10MB）"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = s.config().Engine.TTSProvider
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.Audio))
	if err != nil || len(raw) == 0 {
		writeJSON(w, 400, map[string]any{"error": "需要 audio 字段（base64 编码的样本音频）"})
		return
	}
	if len(raw) > 10<<20 {
		writeJSON(w, 400, map[string]any{"error": "样本音频超过 10MB，剪一段 10~30 秒的就好"})
		return
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "wav"
	}
	params := req.Params
	if params == nil {
		params = map[string]string{}
	}
	// 复刻和合成共用同一份凭据
	cfg := map[string]string{}
	for k, v := range s.config().Engine.TTSProviders[provider] {
		cfg[k] = v
	}
	if _, ok := params["denoise"]; !ok {
		params["denoise"] = "true"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	st, err := s.speech.CloneSubmit(ctx, provider, cfg, audio.CloneRequest{
		SpeakerID: req.SpeakerID,
		Audio:     raw,
		Format:    format,
		Text:      req.Text,
		Params:    params,
	})
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"state":  st,
		"hint":   "训练是异步的，通常 10 秒到 1 分钟；完成后把「模型版本」切到 seed-icl-2.0 才能用它合成。",
		"cfg":    cfg["resource_id"],
		"status": st.Status,
	})
}

// handleTTSCloneStatus 查询复刻状态（前端轮询）。
func (s *server) handleTTSCloneStatus(w http.ResponseWriter, r *http.Request) {
	provider := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider")))
	speaker := strings.TrimSpace(r.URL.Query().Get("speaker_id"))
	if provider == "" {
		provider = s.config().Engine.TTSProvider
	}
	if speaker == "" {
		writeJSON(w, 400, map[string]any{"error": "需要 speaker_id 参数"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	st, err := s.speech.CloneQuery(ctx, provider, s.config().Engine.TTSProviders[provider], speaker)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": st})
}
