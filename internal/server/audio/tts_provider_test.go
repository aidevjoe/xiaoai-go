package audio

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSynth 是可控的 TTS 实现，用来验证路由层的"复用/新建/传音色"。
type fakeSynth struct {
	provider string
	cfg      map[string]string
	voice    string
	speed    float32
	closed   bool
	calls    int
}

func (f *fakeSynth) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return f.SynthesizeWith(ctx, SynthOptions{Text: text})
}

func (f *fakeSynth) SynthesizeWith(_ context.Context, opt SynthOptions) ([]byte, error) {
	f.calls++
	f.voice = opt.Voice
	f.speed = opt.Speed
	return []byte{1, 2, 3, 4}, nil
}

func (f *fakeSynth) TTSRate() int   { return 16000 }
func (f *fakeSynth) Status() string { return "fake(" + f.provider + ")" }
func (f *fakeSynth) Close() error   { f.closed = true; return nil }
func (f *fakeSynth) SetSpeed(v float32) {
	f.speed = v
}
func (f *fakeSynth) Speed() float32 { return f.speed }

// fakeSeq 保证每个测试用独立的 provider 名：
// RegisterTTS 对重名直接 panic（那是代码结构错误），测试里不能复用同一个名字。
var fakeSeq atomic.Int32

// newFakeService 造一个"只装有 fake provider"的路由层。
// 返回的名字就是全局 provider，测试里用它去断言。
func newFakeService(t *testing.T) (*Service, *[]*fakeSynth, string) {
	t.Helper()
	name := fmt.Sprintf("fake-%d", fakeSeq.Add(1))
	var made []*fakeSynth
	RegisterTTS(TTSProvider{
		Name: name, Desc: "测试用", Local: false,
		New: func(opt TTSOptions) (Synthesizer, error) {
			f := &fakeSynth{provider: opt.Provider, cfg: opt.Params}
			made = append(made, f)
			return f, nil
		},
	})
	return NewService(name, map[string]string{"key": "global"}), &made, name
}

// 档案只覆盖音色时，供应商与参数必须继续跟全局。
func TestResolve_ProfileOverridesVoiceOnly(t *testing.T) {
	s, _, name := newFakeService(t)
	s.SetSpeed(1)
	p := s.Resolve(VoiceChoice{Voice: "voice-b"})
	if p.Provider != name {
		t.Fatalf("供应商应跟全局，实际 %q", p.Provider)
	}
	if p.Voice != "voice-b" {
		t.Fatalf("音色应被覆盖，实际 %q", p.Voice)
	}
	if p.Params["key"] != "global" {
		t.Fatalf("参数应跟全局，实际 %v", p.Params)
	}
}

// 档案换了平台时，绝不能把全局平台的密钥带过去（会发给错误的服务商）。
func TestResolve_ProviderSwitchDoesNotLeakGlobalParams(t *testing.T) {
	s, _, _ := newFakeService(t)
	p := s.Resolve(VoiceChoice{Provider: "edge", Voice: "zh-CN-XiaoxiaoNeural",
		Params: map[string]string{"format": "mp3"}})
	if p.Provider != "edge" {
		t.Fatalf("provider = %q", p.Provider)
	}
	if _, ok := p.Params["key"]; ok {
		t.Fatalf("全局豆包密钥不该带到 edge：%v", p.Params)
	}
	if p.Params["format"] != "mp3" {
		t.Fatalf("档案自己的参数应保留：%v", p.Params)
	}
}

// 音色要逐次传下去，而不是改实例状态。
func TestService_PassesVoicePerCall(t *testing.T) {
	s, made, _ := newFakeService(t)
	ctx := context.Background()
	if _, _, err := s.Synthesize(ctx, "你好", VoiceChoice{Voice: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Synthesize(ctx, "你好", VoiceChoice{Voice: "v2"}); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatalf("同一 provider+参数应只建一个实例，实际 %d", len(*made))
	}
	if got := (*made)[0].voice; got != "v2" {
		t.Fatalf("最后一次调用应使用 v2，实际 %q", got)
	}
}

// 参数不同的请求必须各自建实例（账号/端点不同）。
func TestService_SeparatesInstancesByParams(t *testing.T) {
	s, made, name := newFakeService(t)
	ctx := context.Background()
	_, _, _ = s.Synthesize(ctx, "a", VoiceChoice{})
	_, _, _ = s.Synthesize(ctx, "b", VoiceChoice{Provider: name, Params: map[string]string{"key": "other"}})
	if len(*made) != 2 {
		t.Fatalf("参数不同应建两个实例，实际 %d", len(*made))
	}
}

// 引擎里装的正是这套配置时必须复用引擎，不能再建一个（本地模型会重复吃内存）。
func TestService_ReusesEngineForSameConfig(t *testing.T) {
	s, made, name := newFakeService(t)
	eng := &fakeEngine{}
	s.SetEngine(eng, name, map[string]string{"key": "global"})
	ctx := context.Background()
	pcm, rate, err := s.Synthesize(ctx, "你好", VoiceChoice{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) == 0 || rate != 22050 {
		t.Fatalf("应走引擎，实际 pcm=%d rate=%d", len(pcm), rate)
	}
	if len(*made) != 0 {
		t.Fatalf("不该另建实例，实际 %d 个", len(*made))
	}
	if len(eng.voices) != 1 || eng.voices[0] != "" {
		t.Fatalf("音色应原样传给引擎：%v", eng.voices)
	}
}

// 引擎装的是 A，请求要 B：必须新建实例，且不影响引擎。
func TestService_FallsBackToNewInstanceForOtherProvider(t *testing.T) {
	s, made, name := newFakeService(t)
	s.SetEngine(&fakeEngine{}, name, map[string]string{"key": "global"})
	ctx := context.Background()
	if _, _, err := s.Synthesize(ctx, "hi", VoiceChoice{Provider: name, Params: map[string]string{"key": "x"}}); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatalf("参数不同应新建实例，实际 %d", len(*made))
	}
}

// 关掉的 TTS 要返回"能力未加载"，让上层退回设备原生 TTS 而不是静默失败。
func TestService_NoneProvider(t *testing.T) {
	s := NewService("none", nil)
	_, _, err := s.Synthesize(context.Background(), "hi", VoiceChoice{})
	if !errors.Is(err, ErrCapability) {
		t.Fatalf("应返回 ErrCapability，实际 %v", err)
	}
}

// LRU：超过上限的实例要被关闭，不能无限涨。
func TestService_EvictsOldInstances(t *testing.T) {
	s, made, name := newFakeService(t)
	s.limit = 2
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		_, _, _ = s.Synthesize(ctx, "x", VoiceChoice{Provider: name,
			Params: map[string]string{"key": fmt.Sprint(i)}})
	}
	closed := 0
	for _, f := range *made {
		if f.closed {
			closed++
		}
	}
	if closed < 2 {
		t.Fatalf("应淘汰至少 2 个实例，实际关闭 %d", closed)
	}
}

type fakeEngine struct {
	voices []string
}

func (f *fakeEngine) SpeechProb([]byte) (float32, error) { return 0, nil }
func (f *fakeEngine) Transcribe(context.Context, []byte) (string, error) {
	return "", nil
}
func (f *fakeEngine) Synthesize(context.Context, string) ([]byte, error) {
	return []byte{9, 9}, nil
}
func (f *fakeEngine) SynthesizeWith(_ context.Context, opt SynthOptions) ([]byte, error) {
	f.voices = append(f.voices, opt.Voice)
	return []byte{9, 9}, nil
}
func (f *fakeEngine) TTSRate() int                         { return 22050 }
func (f *fakeEngine) DetectKeyword([]byte) (string, error) { return "", nil }
func (f *fakeEngine) Status() string                       { return "fake-engine" }
func (f *fakeEngine) Close() error                         { return nil }

// ---------- 豆包 ----------

// v3 返回是一串连写的 JSON（HTTP Chunked），不能按行切。
func TestParseDoubaoV3_Chunked(t *testing.T) {
	a := base64.StdEncoding.EncodeToString([]byte{1, 2})
	b := base64.StdEncoding.EncodeToString([]byte{3})
	body := `{"code":0,"message":"OK","data":"` + a + `"}{"code":0,"message":"OK","data":"` + b + `"}{"code":20000000,"message":"OK"}`
	got, err := parseDoubaoV3([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("拼接结果不对: %v", got)
	}
}

// SSE 形态（data: {...}）也要能解析，避免换通道就崩。
func TestParseDoubaoV3_SSE(t *testing.T) {
	a := base64.StdEncoding.EncodeToString([]byte{7, 8})
	body := "data: {\"code\":0,\"data\":\"" + a + "\"}\n\ndata: {\"code\":20000000}\n\n"
	got, err := parseDoubaoV3([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 7 {
		t.Fatalf("SSE 解析结果不对: %v", got)
	}
}

// 平台报错要带出 code 与 message，否则只能看到"没声音"。
func TestParseDoubaoV3_ErrorCode(t *testing.T) {
	_, err := parseDoubaoV3([]byte(`{"code":3001,"message":"invalid speaker"}`))
	if err == nil {
		t.Fatal("应报错")
	}
}

// 倍率换豆包刻度：1.0→0，1.5→50，0.4→-50（下限）。
func TestSpeechRateOf(t *testing.T) {
	cases := map[float32]int{1.0: 0, 1.5: 50, 2.0: 100, 0.5: -50, 0.4: -50}
	for in, want := range cases {
		if got := speechRateOf(in); got != want {
			t.Fatalf("speed %.1f → %d，期望 %d", in, got, want)
		}
	}
}

// v1 走"整段 JSON + base64"，验证请求体与解码。
func TestDoubaoV1_Synthesize(t *testing.T) {
	var gotAuth, gotPath string
	pcm := []byte{5, 6, 7, 8}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		fmt.Fprintf(w, `{"code":3000,"message":"Success","data":%q}`,
			base64.StdEncoding.EncodeToString(pcm))
	}))
	defer srv.Close()

	syn, err := NewSynthesizer("doubao", TTSOptions{Params: map[string]string{
		"api_version": "v1", "app_id": "app", "access_key": "tok",
		"endpoint": srv.URL + "/api/v1/tts", "format": "pcm",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer syn.Close()
	out, err := syn.Synthesize(context.Background(), "你好")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(pcm) {
		t.Fatalf("音频不对: %v", out)
	}
	// 分号是硬要求：写成 "Bearer tok" 会 401
	if gotAuth != "Bearer;tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/api/v1/tts" {
		t.Fatalf("path = %q", gotPath)
	}
}

// 缺密钥时要在装配阶段就报错，而不是每句话都失败一次。
func TestDoubao_MissingCredentials(t *testing.T) {
	if _, err := NewSynthesizer("doubao", TTSOptions{Params: map[string]string{"api_version": "v3"}}); err == nil {
		t.Fatal("缺 api_key 应报错")
	}
}

// ---------- Edge ----------

// Sec-MS-GEC 必须与 Python edge-tts 的算法逐位一致，否则握手直接 403。
func TestEdgeSecMSGEC(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	const want = "5176AD838BB5CF9DEF9FC0FBC75C3E1F4BEE2A52954B6AA01494AE5E3CA4F09F"
	if got := edgeSecMSGEC(now); got != want {
		t.Fatalf("Sec-MS-GEC = %s，期望 %s", got, want)
	}
	// 同一 5 分钟窗口内必须相同（否则每次请求都要重新算，且容易踩到边界）
	if a, b := edgeSecMSGEC(now), edgeSecMSGEC(now.Add(4*time.Minute)); a != b {
		t.Fatalf("同一窗口内令牌应稳定: %s != %s", a, b)
	}
	if a, b := edgeSecMSGEC(now), edgeSecMSGEC(now.Add(5*time.Minute)); a == b {
		t.Fatal("跨窗口后令牌应变化")
	}
}

// 二进制帧：前 2 字节是大端头长度，之后才是头，再之后才是音频。
func TestEdgeBinaryPayload(t *testing.T) {
	header := []byte("X-RequestId:abc\r\nPath:audio\r\n")
	frame := make([]byte, 2+len(header)+3)
	frame[0] = byte(len(header) >> 8)
	frame[1] = byte(len(header))
	copy(frame[2:], header)
	copy(frame[2+len(header):], []byte{1, 2, 3})

	got, ok := edgeBinaryPayload(frame)
	if !ok || len(got) != 3 || got[0] != 1 {
		t.Fatalf("解析失败: ok=%v got=%v", ok, got)
	}
	// 非音频帧（turn.end）不该被当成音频
	if _, ok := edgeBinaryPayload([]byte{0, 4, 'a', 'b', 'c', 'd'}); ok {
		t.Fatal("非 audio 帧不应返回 payload")
	}
}

func TestEdgeRate(t *testing.T) {
	cases := map[float32]string{1: "+0%", 1.2: "+20%", 0.5: "-50%", 3: "+100%", 0.1: "-50%"}
	for in, want := range cases {
		if got := edgeRate(in); got != want {
			t.Fatalf("speed %.1f → %s，期望 %s", in, got, want)
		}
	}
}

func TestSSML(t *testing.T) {
	if got := ssmlLang("zh-CN-XiaoxiaoNeural"); got != "zh-CN" {
		t.Fatalf("lang = %q", got)
	}
	got := xmlEscape(`Tom & Jerry's <b>`)
	want := "Tom &amp; Jerry&apos;s &lt;b&gt;"
	if got != want {
		t.Fatalf("转义 = %q，期望 %q", got, want)
	}
	// 控制字符会让服务端直接拒收
	if s := sanitizeForSSML("a\x00b\nc"); s != "a b c" {
		t.Fatalf("清洗 = %q", s)
	}
}

// 档案把助手切到另一个平台时，要用"那个平台自己的全局配置"打底。
// 密钥是账号级的：用户只在引擎页配一次，不该在每个助手里重填。
func TestResolve_ProfileProviderUsesThatProviderGlobalConfig(t *testing.T) {
	s, _, name := newFakeService(t)
	s.SetProviderConfigs(map[string]map[string]string{
		name:   {"key": "global", "region": "cn"},
		"edge": {"format": "mp3"},
	})
	p := s.Resolve(VoiceChoice{Provider: "edge", Voice: "zh-CN-XiaoxiaoNeural"})
	if p.Params["format"] != "mp3" {
		t.Fatalf("应带上 edge 自己的配置: %v", p.Params)
	}
	if _, ok := p.Params["key"]; ok {
		t.Fatalf("不该混入别的平台的密钥: %v", p.Params)
	}
	// 档案自己填的参数要能覆盖平台配置（换账号的场景）
	p2 := s.Resolve(VoiceChoice{Provider: "edge", Params: map[string]string{"format": "pcm"}})
	if p2.Params["format"] != "pcm" {
		t.Fatalf("档案参数应覆盖平台配置: %v", p2.Params)
	}
}

// 用户手工登记的音色清单要和内置清单合并，且 format 两种写法都要认。
func TestCustomVoicesFromParams(t *testing.T) {
	cfg := map[string]string{
		"voice_list":   "zh_male_a:低沉男声\nzh_female_b：温柔女声\nzh_x_c",
		"clone_voices": "S_abc:我的声音",
	}
	got := customVoicesFromParams(cfg)
	byID := map[string]Voice{}
	for _, v := range got {
		byID[v.ID] = v
	}
	if len(got) != 4 {
		t.Fatalf("应有 4 条，实际 %d: %v", len(got), got)
	}
	if v := byID["zh_male_a"]; v.Name != "低沉男声" || v.Cloned {
		t.Fatalf("英文冒号+名字解析错: %+v", v)
	}
	if v := byID["zh_female_b"]; v.Name != "温柔女声" {
		t.Fatalf("全角冒号解析错: %+v", v)
	}
	if v := byID["zh_x_c"]; v.Name != "zh_x_c" {
		t.Fatalf("只有 ID 时应回填名字: %+v", v)
	}
	if v := byID["S_abc"]; !v.Cloned {
		t.Fatalf("复刻音色要打 Cloned 标记: %+v", v)
	}
}
