package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ToolExecutor 执行工具调用。返回的字符串会作为 tool 消息喂回模型。
type ToolExecutor interface {
	Specs() []Tool
	Exec(ctx context.Context, name, argsJSON string) (string, error)
}

// Runner 负责"模型要工具 → 执行 → 再问模型"的循环。
type Runner struct {
	Client    *Client
	Exec      ToolExecutor
	System    string
	MaxRounds int // 默认 3：再多通常是模型在打转
}

// Run 执行一轮用户输入，返回最终文本与会话历史。
//
// onDelta 会在模型输出文本时实时回调，调用方可以据此做句子切分 + 提前合成。
func (r *Runner) Run(ctx context.Context, history []Message, userText string, onDelta func(string)) (Result, []Message, error) {
	return r.RunWithSystem(ctx, r.System, history, userText, onDelta)
}

// RunWithSystem 用指定的 system 提示词跑一轮（可把预取的上下文拼进去）。
func (r *Runner) RunWithSystem(ctx context.Context, system string, history []Message, userText string, onDelta func(string)) (Result, []Message, error) {
	maxRounds := r.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 3
	}

	msgs := make([]Message, 0, len(history)+2)
	if system != "" {
		msgs = append(msgs, Message{Role: "system", Content: system})
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, Message{Role: "user", Content: userText})

	var specs []Tool
	if r.Exec != nil {
		specs = r.Exec.Specs()
	}

	res := Result{}
	for round := 1; round <= maxRounds; round++ {
		res.Rounds = round
		t0 := time.Now()
		text, calls, err := r.Client.Stream(ctx, msgs, specs, onDelta)
		res.RoundMS = append(res.RoundMS, time.Since(t0).Milliseconds())
		if err != nil {
			return res, msgs[1:], err
		}

		if len(calls) == 0 {
			res.Text = text
			msgs = append(msgs, Message{Role: "assistant", Content: text})
			return res, msgs[1:], nil
		}

		// 模型要调工具：把 assistant 的 tool_calls 原样带回，再逐条执行
		res.ToolCalls = calls
		res.ToolRounds++
		msgs = append(msgs, Message{Role: "assistant", Content: text, ToolCalls: calls})
		for _, c := range calls {
			out, err := r.Exec.Exec(ctx, c.Function.Name, c.Function.Arguments)
			if err != nil {
				out = fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
			}
			msgs = append(msgs, Message{
				Role:       "tool",
				ToolCallID: c.ID,
				Content:    out,
			})
		}
	}

	// 轮数用尽：不再给工具，逼它用人话收尾
	text, _, err := r.Client.Stream(ctx, msgs, nil, onDelta)
	if err != nil {
		return res, msgs[1:], err
	}
	res.Text = text
	msgs = append(msgs, Message{Role: "assistant", Content: text})
	return res, msgs[1:], nil
}

// Args 把工具参数字符串解析成 map，便于日志与校验。
func Args(s string) map[string]any {
	m := map[string]any{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}
