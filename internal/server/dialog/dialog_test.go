package dialog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
)

// fakeLLM 记录请求并按固定文本流式返回。
type fakeLLM struct {
	*httptest.Server
	mu       sync.Mutex
	models   []string
	lastMsgs []map[string]any
	// Delay 模拟"模型很慢"，用于测试慢响应场景
	Delay time.Duration
}

func newFakeLLM(t *testing.T, reply string) *fakeLLM {
	f := &fakeLLM{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		f.mu.Lock()
		if m, ok := body["model"].(string); ok {
			f.models = append(f.models, m)
		}
		if msgs, ok := body["messages"].([]any); ok {
			f.lastMsgs = f.lastMsgs[:0]
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					f.lastMsgs = append(f.lastMsgs, mm)
				}
			}
		}
		f.mu.Unlock()

		// 模拟慢模型：首字迟迟不来
		if d := f.Delay; d > 0 {
			time.Sleep(d)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		// 拆成两个增量，验证流式
		for _, part := range []string{reply[:len(reply)/2], reply[len(reply)/2:]} {
			b, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": part}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLLM) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.models)
}

// fakeTarget 记录被播放的 PCM。
type fakeTarget struct {
	mu     sync.Mutex
	chunks [][]byte
	rates  []int
}

func (f *fakeTarget) PlayPCM(ctx context.Context, pcm []byte, rate int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	f.chunks = append(f.chunks, cp)
	f.rates = append(f.rates, rate)
	return nil
}

func (f *fakeTarget) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chunks)
}

func testRegistry(t *testing.T, aURL, bURL string) *agent.Registry {
	reg, err := agent.NewRegistry([]*agent.Profile{
		{
			ID: "steward", Name: "管家", WakeWords: []string{"小黑", "你好小黑"},
			SystemPrompt: "你是家庭管家",
			LLM:          agent.LLM{BaseURL: aURL, Model: "model-a"},
		},
		{
			ID: "chat", Name: "闲聊", WakeWords: []string{"小智"},
			SystemPrompt: "你是聊天伙伴",
			LLM:          agent.LLM{BaseURL: bURL, Model: "model-b"},
			Tools:        []string{"none"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// 关键测试：不同唤醒词 → 打到不同的模型端点。
func TestWakeWordRoutesToDifferentLLM(t *testing.T) {
	llmA := newFakeLLM(t, "好的，灯已打开。")
	llmB := newFakeLLM(t, "今天心情不错呢。")

	reg := testRegistry(t, llmA.URL, llmB.URL)
	eng := audio.NewMockEngine(0.9, "x")
	mgr := NewManager(reg, nil, eng, log.New(io.Discard, "", 0))
	tgt := &fakeTarget{}
	sess := mgr.Session("dev1", tgt)

	ctx := context.Background()

	// 说"小黑" → 应走 A
	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	if got := sess.ProfileID(); got != "steward" {
		t.Fatalf("应选 steward，实际 %s", got)
	}
	turn, err := sess.HandleText(ctx, "打开客厅的灯")
	if err != nil {
		t.Fatal(err)
	}
	if turn.ProfileID != "steward" {
		t.Fatalf("轮次档案不对: %s", turn.ProfileID)
	}
	if llmA.hitCount() != 1 || llmB.hitCount() != 0 {
		t.Fatalf("请求打错了端点: A=%d B=%d", llmA.hitCount(), llmB.hitCount())
	}
	if turn.Reply != "好的，灯已打开。" {
		t.Fatalf("回复不对: %q", turn.Reply)
	}
	if tgt.count() == 0 {
		t.Fatal("应该有音频被推到设备")
	}
	if turn.FirstDeltaMS < 0 {
		t.Fatal("首字耗时不应为负")
	}

	// 说"小智" → 应走 B，且历史被清空
	if err := sess.Wake(ctx, "小智"); err != nil {
		t.Fatal(err)
	}
	if got := sess.ProfileID(); got != "chat" {
		t.Fatalf("应切到 chat，实际 %s", got)
	}
	turn2, err := sess.HandleText(ctx, "今天过得怎么样")
	if err != nil {
		t.Fatal(err)
	}
	if turn2.ProfileID != "chat" {
		t.Fatalf("第二轮档案不对: %s", turn2.ProfileID)
	}
	if llmB.hitCount() != 1 {
		t.Fatalf("应打到 B 端点，实际 %d", llmB.hitCount())
	}
	// 换档案应清空历史：第二次请求只应包含 system + user
	if len(llmB.lastMsgs) != 2 {
		t.Fatalf("换档案后应清空历史，实际消息数 %d", len(llmB.lastMsgs))
	}
}

func TestExitWordSkipsLLM(t *testing.T) {
	llmA := newFakeLLM(t, "不该被调用")
	reg := testRegistry(t, llmA.URL, llmA.URL)
	eng := audio.NewMockEngine(0.9, "x")
	mgr := NewManager(reg, nil, eng, log.New(io.Discard, "", 0))
	sess := mgr.Session("dev2", &fakeTarget{})
	ctx := context.Background()

	sess.Wake(ctx, "小黑")
	before := llmA.hitCount()
	turn, err := sess.HandleText(ctx, "退下")
	if err != nil {
		t.Fatal(err)
	}
	if llmA.hitCount() != before {
		t.Fatal("退出词不应调用模型")
	}
	if turn.Reply != "(退出)" {
		t.Fatalf("应标记退出，实际 %q", turn.Reply)
	}
	if sess.State() != StateIdle {
		t.Fatalf("退出后应回到 idle，实际 %s", sess.State())
	}
}

func TestSplitFirst(t *testing.T) {
	cases := []struct {
		in, cut, tail string
	}{
		{"你好。世界", "你好。", "世界"},
		{"真的吗？！我不信", "真的吗？！", "我不信"},
		{"没有标点", "", "没有标点"},
		{"", "", ""},
	}
	for _, c := range cases {
		cut, tail := splitFirst(c.in, DefaultMaxSentenceRunes)
		if cut != c.cut || tail != c.tail {
			t.Errorf("splitFirst(%q) = (%q,%q)，期望 (%q,%q)", c.in, cut, tail, c.cut, c.tail)
		}
	}
}

func TestSplitFirstForcesCutOnLongSentence(t *testing.T) {
	long := strings.Repeat("啊", 30)
	cut, tail := splitFirst(long, DefaultMaxSentenceRunes)
	if cut == "" {
		t.Fatal("超长无标点必须强制切分，否则首句永远等不到")
	}
	if len([]rune(cut)) > DefaultMaxSentenceRunes {
		t.Fatalf("切出的片段过长: %d", len([]rune(cut)))
	}
	if len([]rune(cut))+len([]rune(tail)) != 30 {
		t.Fatal("切分不应丢字")
	}
}

func TestSplitFirstPrefersComma(t *testing.T) {
	long := strings.Repeat("字", 15) + "，" + strings.Repeat("句", 15)
	cut, _ := splitFirst(long, DefaultMaxSentenceRunes)
	if !strings.HasSuffix(cut, "，") {
		t.Fatalf("超长时应优先在逗号处切，实际: %q", cut)
	}
}

func TestStreamSpeakerOrdersAndPlays(t *testing.T) {
	eng := audio.NewMockEngine(0.9, "x")
	tgt := &fakeTarget{}
	ctx := context.Background()
	sp := NewStreamSpeaker(ctx, audio.EngineSpeaker(eng), tgt)

	// 分片喂入，模拟模型逐字输出
	for _, d := range []string{"好的，", "已经为你", "打开灯。", "还需要", "别的吗？"} {
		sp.Write(d)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if len(eng.TTSInput) != 2 {
		t.Fatalf("应合成 2 句，实际 %d: %v", len(eng.TTSInput), eng.TTSInput)
	}
	if eng.TTSInput[0] != "好的，已经为你打开灯。" {
		t.Fatalf("首句不对: %q", eng.TTSInput[0])
	}
	if tgt.count() != 2 {
		t.Fatalf("应播放 2 段，实际 %d", tgt.count())
	}
}

func TestSessionExpires(t *testing.T) {
	reg := testRegistry(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	sess := mgr.Session("dev3", &fakeTarget{})
	sess.Wake(context.Background(), "小黑")

	if sess.Expired(time.Minute) {
		t.Fatal("刚唤醒不该超时")
	}
	// 手动把活跃时间拨回到 2 分钟前
	sess.mu.Lock()
	sess.lastActive = time.Now().Add(-2 * time.Minute)
	sess.mu.Unlock()
	if !sess.Expired(time.Minute) {
		t.Fatal("应判定超时")
	}
}

// 首句要切得比后续句子短：出声时间只取决于第一句，压缩它能明显提前开口。
func TestFirstLimitShorterThanRest(t *testing.T) {
	if got := firstLimit(20); got != 12 {
		t.Fatalf("20 字上限时首句应为 12 字，实际 %d", got)
	}
	if got := firstLimit(8); got != 8 {
		t.Fatalf("下限不能低于 8 字，实际 %d", got)
	}
	if got := firstLimit(60); got != 36 {
		t.Fatalf("应取 3/5，实际 %d", got)
	}
	// 首句上限不得超过整体上限
	if got := firstLimit(10); got > 10 {
		t.Fatalf("首句上限不能超过整体上限，实际 %d", got)
	}
}
