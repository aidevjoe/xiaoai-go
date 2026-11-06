package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer 起一个假模型服务，按 chunks 依次返回。
type sseServer struct {
	*httptest.Server
	requests []map[string]any
	chunks   [][]string
	idx      int
}

func newSSEServer(t *testing.T, chunks ...[]string) *sseServer {
	s := &sseServer{chunks: chunks}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.requests = append(s.requests, body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if s.idx >= len(s.chunks) {
			s.idx = len(s.chunks) - 1
		}
		for _, line := range s.chunks[s.idx] {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		s.idx++
	}))
	t.Cleanup(s.Close)
	return s
}

func newClient(url string) *Client {
	return New(Config{BaseURL: url, APIKey: "test", Model: "m"})
}

func TestStreamText(t *testing.T) {
	srv := newSSEServer(t, []string{
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"，世界"}}]}`,
	})
	var deltas []string
	text, calls, err := newClient(srv.URL).Stream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if text != "你好，世界" {
		t.Fatalf("文本不对: %q", text)
	}
	if len(calls) != 0 {
		t.Fatalf("不该有工具调用: %+v", calls)
	}
	// 关键：增量必须实时回调，TTS 才能提前开始
	if strings.Join(deltas, "") != text {
		t.Fatalf("增量拼起来应等于全文: %v", deltas)
	}
	if len(deltas) != 2 {
		t.Fatalf("应收到 2 个增量，实际 %d", len(deltas))
	}
}

func TestStreamToolCallIsAggregated(t *testing.T) {
	// 同一个工具调用的 name/id 在第一片，arguments 分三片拼出来
	srv := newSSEServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ha_call_service","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"domain\":\"light\","}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"service\":\"turn_on\"}"}}]}}]}`,
	})
	_, calls, err := newClient(srv.URL).Stream(context.Background(),
		[]Message{{Role: "user", Content: "开灯"}}, []Tool{{Type: "function"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("应聚合为 1 个调用，实际 %d", len(calls))
	}
	c := calls[0]
	if c.Function.Name != "ha_call_service" || c.ID != "call_1" {
		t.Fatalf("name/id 聚合错误: %+v", c)
	}
	want := `{"domain":"light","service":"turn_on"}`
	if c.Function.Arguments != want {
		t.Fatalf("参数拼接错误:\n got %s\nwant %s", c.Function.Arguments, want)
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	_, _, err := newClient(srv.URL).Stream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应返回 401 错误，实际 %v", err)
	}
}

func TestExtraBodyInjected(t *testing.T) {
	srv := newSSEServer(t, []string{`{"choices":[{"delta":{"content":"ok"}}]}`})
	c := New(Config{
		BaseURL: srv.URL, Model: "m",
		Extra: map[string]any{"enable_thinking": false},
	})
	if _, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	body := srv.requests[0]
	v, ok := body["enable_thinking"]
	if !ok {
		t.Fatalf("extra 字段未透传: %+v", body)
	}
	if v != false {
		t.Fatalf("enable_thinking 应为 false，实际 %v", v)
	}
	if body["stream"] != true {
		t.Fatal("必须用流式")
	}
}

// mockExec 记录被执行的工具调用。
type mockExec struct {
	calls []string
	specs []Tool
}

func (m *mockExec) Specs() []Tool { return m.specs }

func (m *mockExec) Exec(ctx context.Context, name, args string) (string, error) {
	m.calls = append(m.calls, name+":"+args)
	return `{"ok":true,"state":"on"}`, nil
}

func TestRunnerToolRoundThenAnswer(t *testing.T) {
	srv := newSSEServer(t,
		// 第一轮：模型要调工具
		[]string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"ha_call_service","arguments":"{\"domain\":\"light\"}"}}]}}]}`,
		},
		// 第二轮：给出最终回答
		[]string{
			`{"choices":[{"delta":{"content":"好的，"}}]}`,
			`{"choices":[{"delta":{"content":"灯已打开"}}]}`,
		},
	)

	exec := &mockExec{specs: []Tool{{Type: "function"}}}
	r := &Runner{Client: newClient(srv.URL), Exec: exec, System: "你是助手", MaxRounds: 3}

	var streamed strings.Builder
	res, history, err := r.Run(context.Background(), nil, "打开灯", func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("应执行 1 次工具，实际 %v", exec.calls)
	}
	if res.Text != "好的，灯已打开" {
		t.Fatalf("最终文本不对: %q", res.Text)
	}
	if res.Rounds != 2 {
		t.Fatalf("应两轮，实际 %d", res.Rounds)
	}
	if streamed.String() != "好的，灯已打开" {
		t.Fatalf("流式输出不对: %q", streamed.String())
	}
	// 历史应包含 user / assistant(tool_calls) / tool / assistant
	if len(history) != 4 {
		t.Fatalf("历史条数应为 4，实际 %d: %+v", len(history), history)
	}
	if history[2].Role != "tool" || history[2].ToolCallID != "c1" {
		t.Fatalf("tool 消息不对: %+v", history[2])
	}
	// 第二轮回传时应带上工具结果
	second := srv.requests[1]
	msgs := second["messages"].([]any)
	if len(msgs) < 4 {
		t.Fatalf("第二次请求消息过少: %d", len(msgs))
	}
}

func TestRunnerStopsAfterMaxRounds(t *testing.T) {
	// 每轮都只要工具，模拟模型打转
	loop := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"cX","type":"function","function":{"name":"loop","arguments":"{}"}}]}}]}`,
	}
	// 前两轮给工具调用，第三轮（兜底、不带 tools）给文本
	srv := newSSEServer(t, loop, loop,
		[]string{`{"choices":[{"delta":{"content":"算了"}}]}`},
	)
	exec := &mockExec{}
	r := &Runner{Client: newClient(srv.URL), Exec: exec, MaxRounds: 2}
	res, _, err := r.Run(context.Background(), nil, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if exec.calls == nil || len(exec.calls) > 2 {
		t.Fatalf("最多执行 2 轮工具，实际 %d", len(exec.calls))
	}
	if res.Text == "" {
		t.Fatal("轮数用尽后应给出兜底文本")
	}
}

// 多供应商常见问题：某个端点不认识我们透传的 extra 字段并返回 400。
// 必须自动去掉 extra 重试，否则用户换模型就整个助手不可用。
func TestRetryWithoutExtraOn400(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, bad := body["enable_thinking"]; bad {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"request rejected by internal MaaS component"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好的\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "m", Extra: map[string]any{"enable_thinking": false}})
	text, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("应自动降级重试成功，实际: %v", err)
	}
	if text != "好的" {
		t.Fatalf("文本不对: %q", text)
	}
	if attempts != 2 {
		t.Fatalf("应请求两次（原始 + 去 extra 重试），实际 %d", attempts)
	}
}

func TestNoDuplicateRetryWithoutExtra(t *testing.T) {
	// 没有 extra 时不该把同一个请求重复发三遍（会白烧 token）
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "m"})
	if _, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, nil); err == nil {
		t.Fatal("应返回错误")
	}
	// 每档只降一样：完整 → 去 temperature → 最小
	if attempts != 3 {
		t.Fatalf("应为 3 次（完整 / 去 temperature / 最小），实际 %d 次", attempts)
	}
}
