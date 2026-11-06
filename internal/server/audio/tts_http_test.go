package audio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 这条测试就是"接一个新的 TTS API"的样板：
// 一个 HTTP 服务 + 一份参数，不碰任何别的代码。
func TestHTTPTTS_PlaysWAVFromService(t *testing.T) {
	var got struct {
		Text  string  `json:"text"`
		Voice string  `json:"voice"`
		Speed float32 `json:"speed"`
	}
	pcm := make([]byte, 3200) // 0.1 秒 16k 静音，够判断长度
	for i := range pcm {
		pcm[i] = byte(i % 7)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("方法 = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(EncodeWAV(pcm, 16000))
	}))
	defer srv.Close()

	syn, err := NewSynthesizer("http", TTSOptions{
		Speed:  1.2,
		Params: map[string]string{"url": srv.URL, "voice": "zh-CN-Xiaoxiao"},
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer syn.Close()

	out, err := syn.Synthesize(context.Background(), "打开客厅的灯")
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	if string(out) != string(pcm) {
		t.Fatalf("返回的 PCM 与 WAV 里的不一致: %d vs %d 字节", len(out), len(pcm))
	}
	// 采样率以响应头为准（配置写错也不该变声）
	if syn.TTSRate() != 16000 {
		t.Fatalf("采样率 = %d，应取 WAV 头里的 16000", syn.TTSRate())
	}
	if got.Text != "打开客厅的灯" || got.Voice != "zh-CN-Xiaoxiao" || got.Speed != 1.2 {
		t.Fatalf("请求体不对: %+v", got)
	}
}

// 接口挂掉时必须给出人能看懂的错误，并且把原因记进 Status（后台能看到）。
func TestHTTPTTS_ServiceErrorIsVisible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "模型正在加载", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	syn, err := NewSynthesizer("http", TTSOptions{Params: map[string]string{"url": srv.URL}})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := syn.Synthesize(context.Background(), "hi"); err == nil {
		t.Fatal("接口 503 时应报错")
	} else if !contains(err.Error(), "503") || !contains(err.Error(), "模型正在加载") {
		t.Fatalf("错误信息应包含状态码与对端原因，实际: %v", err)
	}
	if !contains(syn.Status(), "上次失败") {
		t.Fatalf("Status 应带上次失败原因，实际: %s", syn.Status())
	}
}

// 对端卡住时不能一直挂着把对话拖死。
func TestHTTPTTS_TimeoutIsRespected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write(EncodeWAV(make([]byte, 320), 16000))
	}))
	defer srv.Close()

	syn, err := NewSynthesizer("http", TTSOptions{Params: map[string]string{
		"url": srv.URL, "timeout_ms": "50",
	}})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	start := time.Now()
	if _, err := syn.Synthesize(context.Background(), "hi"); err == nil {
		t.Fatal("超时应该报错")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("超时没生效，等了 %v", time.Since(start))
	}
}

func TestHTTPTTS_MissingURLFailsFast(t *testing.T) {
	if _, err := NewSynthesizer("http", TTSOptions{}); err == nil {
		t.Fatal("缺 url 应立刻报错，而不是等到合成时")
	}
}

// 双声道 WAV 要降成单声道，否则播放出来是两倍速的噪声。
func TestDecodeWAV_DownmixStereo(t *testing.T) {
	const n = 100
	body := make([]byte, 44+n*4)
	copy(body[0:4], "RIFF")
	binary.LittleEndian.PutUint32(body[4:8], uint32(36+n*4))
	copy(body[8:12], "WAVE")
	copy(body[12:16], "fmt ")
	binary.LittleEndian.PutUint32(body[16:20], 16)
	binary.LittleEndian.PutUint16(body[20:22], 1)
	binary.LittleEndian.PutUint16(body[22:24], 2) // 立体声
	binary.LittleEndian.PutUint32(body[24:28], 44100)
	binary.LittleEndian.PutUint32(body[28:32], 44100*4)
	binary.LittleEndian.PutUint16(body[32:34], 4)
	binary.LittleEndian.PutUint16(body[34:36], 16)
	copy(body[36:40], "data")
	binary.LittleEndian.PutUint32(body[40:44], uint32(n*4))
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(body[44+i*4:], uint16(int16(1000)))
		binary.LittleEndian.PutUint16(body[46+i*4:], uint16(int16(3000)))
	}

	pcm, rate, err := decodeWAV(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if rate != 44100 {
		t.Fatalf("采样率 = %d", rate)
	}
	if len(pcm) != n*2 {
		t.Fatalf("单声道长度 = %d，应为 %d", len(pcm), n*2)
	}
	// (1000+3000)/2 = 2000
	if got := int16(binary.LittleEndian.Uint16(pcm[0:2])); got != 2000 {
		t.Fatalf("降混结果 = %d，应为 2000", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
