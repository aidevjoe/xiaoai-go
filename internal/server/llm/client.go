// Package llm 是 OpenAI 兼容的流式对话客户端。
//
// 只做三件事：
//  1. 发请求（SSE 流式），把每个文本增量回调出去，让 TTS 能尽早开始
//  2. 聚合 tool_calls 分片（同一个调用会分散在多个 delta 里，必须按 index 累积）
//  3. 多轮工具调用循环
//
// 不引入任何第三方 SDK：标准库够用，且能精确控制 extra 字段
// （例如关掉思考模式）。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Config 是一次模型调用的配置。
type Config struct {
	BaseURL     string // 例如 https://tokenhub.tencentmaas.com/v1
	APIKey      string
	Model       string
	Temperature float32
	MaxTokens   int
	Timeout     time.Duration
	Extra       map[string]any // 透传字段，如 {"enable_thinking": false}
}

func (c Config) normalized() Config {
	if c.Temperature == 0 {
		c.Temperature = 0.4
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 400
	}
	if c.Timeout == 0 {
		c.Timeout = 60 * time.Second
	}
	return c
}

// Message 是对话消息。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall 是一次函数调用。
type ToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function Func   `json:"function"`
}

// Func 是函数名与参数（参数是 JSON 字符串，流式下分片到达）。
type Func struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Tool 是给模型的工具定义。
type Tool struct {
	Type     string   `json:"type"`
	Function ToolSpec `json:"function"`
}

// ToolSpec 描述工具名、用途与参数 schema。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Result 是一次（含工具轮次）调用的结果。
type Result struct {
	Text      string
	ToolCalls []ToolCall // 仅最后被执行的工具调用，供日志/后台展示
	Rounds    int
	// RoundMS 记录每一轮模型调用耗时（毫秒）。
	// 有工具调用时是两轮：第一轮决定调什么工具，第二轮才出文本。
	// 拆开看才知道慢在"决策"还是"生成"。
	RoundMS []int64
	// ToolRounds 是真正执行了工具的轮数（0 表示模型直接回答）。
	// Rounds 是模型调用次数，两者差 1——后台要把这个区别说清楚，
	// 否则"1 轮"会被误读成"调了一次工具"。
	ToolRounds int
}

// Client 是模型客户端。
type Client struct {
	cfg Config
	hc  *http.Client
	// lvl 记录该端点已经验证过的降级档位，后续请求直接从这档开始，
	// 不必每次都重走失败的档位（glm 拒绝 temperature、kimi 也一样）。
	lvl atomic.Int32
}

// New 构造客户端。复用 http.Client 以保持连接（省掉每次 TLS 握手）。
func New(cfg Config) *Client {
	cfg = cfg.normalized()
	return &Client{
		cfg: cfg,
		hc: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  false,
			},
		},
	}
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Stream      bool      `json:"stream"`
	Temperature float32   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// streamDelta 是 SSE 里一个 chunk 的结构（只取需要的字段）。
type streamDelta struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Stream 发起一次流式请求。
//
// 多供应商兼容策略：不同端点对参数的支持差异很大（例如 kimi 不接受 temperature、
// 某些端点不认识我们透传的 extra）。遇到 400 时按"逐级降级"重试，
// 而不是直接让整个助手不可用：
//
//  1. 完整请求（含 extra / temperature / max_tokens）
//  2. 去掉 extra
//  3. 只保留 model + messages + stream（最小可用请求）
func (c *Client) Stream(ctx context.Context, msgs []Message, tools []Tool, onDelta func(string)) (string, []ToolCall, error) {
	body := chatRequest{
		Model:       c.cfg.Model,
		Messages:    msgs,
		Tools:       tools,
		Stream:      true,
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}
	if len(tools) > 0 {
		body.ToolChoice = "auto"
	}

	type variant struct {
		extra   map[string]any
		noTemp  bool
		minimal bool
	}

	// 四档降级，从已经验证过的档位开始：
	// 0=完整  1=去 extra  2=再去 temperature  3=最小
	ladder := []variant{{c.cfg.Extra, false, false}}
	if len(c.cfg.Extra) > 0 {
		ladder = append(ladder, variant{nil, false, false})
	}
	if c.cfg.Temperature != 0 {
		ladder = append(ladder, variant{nil, true, false})
	}
	ladder = append(ladder, variant{nil, true, true})
	startLvl := int(c.lvl.Load())
	if startLvl >= len(ladder) {
		startLvl = len(ladder) - 1
	}

	var lastErr error
	for i := startLvl; i < len(ladder); i++ {
		if i > startLvl && lastErr != nil && !strings.Contains(lastErr.Error(), "HTTP 400") {
			break // 不是请求体问题，降级也没用
		}
		v := ladder[i]
		text, calls, err := c.attempt(ctx, &body, v.extra, v.noTemp, v.minimal, onDelta)
		if err == nil {
			c.lvl.Store(int32(i))
			return text, calls, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", nil, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm: 未知错误")
	}
	return "", nil, lastErr
}

func (c *Client) attempt(ctx context.Context, body *chatRequest, extra map[string]any, noTemp, minimal bool, onDelta func(string)) (string, []ToolCall, error) {
	b := *body
	if noTemp {
		b.Temperature = 0 // omitempty：为 0 时不会出现在请求体里
	}
	if minimal {
		// 只留最保险的字段
		b = chatRequest{Model: body.Model, Messages: body.Messages, Stream: true}
	}
	raw, err := c.encode(&b, extra)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return parseSSE(resp.Body, onDelta)
}

// encode 把请求编码成 JSON；extra 非空时合并进去（覆盖同名键）。
func (c *Client) encode(body *chatRequest, extra map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// parseSSE 解析服务端事件流。
func parseSSE(r io.Reader, onDelta func(string)) (string, []ToolCall, error) {
	var text strings.Builder
	// 同一个工具调用会分散在多个 chunk，按 index 聚合
	calls := map[int]*ToolCall{}
	var order []int

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var d streamDelta
		if err := json.Unmarshal([]byte(payload), &d); err != nil {
			continue // 忽略心跳/注释等非 JSON 行
		}
		if d.Error != nil {
			return text.String(), nil, fmt.Errorf("llm: 模型返回错误: %s", d.Error.Message)
		}
		for _, ch := range d.Choices {
			if ch.Delta.Content != "" {
				text.WriteString(ch.Delta.Content)
				if onDelta != nil {
					onDelta(ch.Delta.Content)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := tc.Index
				cur, ok := calls[idx]
				if !ok {
					cur = &ToolCall{Index: idx}
					calls[idx] = cur
					order = append(order, idx)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Type != "" {
					cur.Type = tc.Type
				}
				// name 只在第一片出现；arguments 逐片拼接
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					cur.Function.Arguments += tc.Function.Arguments
				}
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return text.String(), nil, err
	}

	out := make([]ToolCall, 0, len(order))
	for _, i := range order {
		out = append(out, *calls[i])
	}
	return text.String(), out, nil
}
