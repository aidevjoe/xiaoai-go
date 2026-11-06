package dialog

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"xiaoai-go/internal/server/audio"
)

// 隔太久再说话时不该把旧对话带进提示词：既费 token，又会让模型
// 把上一轮的话题当成当前语境。
func TestContextExpiresAfterTTL(t *testing.T) {
	llm := newFakeLLM(t, "好的。")
	ag := testRegistry(t, llm.URL, llm.URL)
	mgr := NewManager(ag, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	mgr.ContextTTL = 30 * time.Minute
	tgt := &fakeTarget{}
	sess := mgr.Session("dev-ttl", tgt)
	ctx := context.Background()

	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.HandleText(ctx, "打开客厅的灯"); err != nil {
		t.Fatal(err)
	}

	// 第二句紧接着说：应该带上历史（system + user + assistant + user）
	if _, err := sess.HandleText(ctx, "再把空调打开"); err != nil {
		t.Fatal(err)
	}
	llm.mu.Lock()
	n := len(llm.lastMsgs)
	llm.mu.Unlock()
	if n < 4 {
		t.Fatalf("紧接着的对话应带上历史，实际只有 %d 条消息", n)
	}

	// 把上次活跃时间拨回 40 分钟前，模拟"隔了很久才说话"
	sess.mu.Lock()
	sess.lastActive = time.Now().Add(-40 * time.Minute)
	sess.mu.Unlock()

	if _, err := sess.HandleText(ctx, "今天天气怎么样"); err != nil {
		t.Fatal(err)
	}
	llm.mu.Lock()
	msgs := append([]map[string]any(nil), llm.lastMsgs...)
	llm.mu.Unlock()
	if len(msgs) != 2 {
		t.Fatalf("超过保鲜期后应只发 system+user 两条，实际 %d 条", len(msgs))
	}
	for _, m := range msgs {
		if s, _ := m["content"].(string); strings.Contains(s, "打开客厅的灯") {
			t.Fatal("过期后不该再把旧对话带进提示词")
		}
	}
}
