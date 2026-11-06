package dialog

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"xiaoai-go/internal/server/audio"
)

// 端到端确认：后台把档案命名为「管家」后，真正发给模型的 system 消息里
// 带着这个身份，问「你是谁」时模型才答得出「我是管家」。
func TestHandleText_PutsDisplayNameIntoSystemMessage(t *testing.T) {
	llm := newFakeLLM(t, "我是管家，有事尽管吩咐。")
	reg := testRegistry(t, llm.URL, llm.URL) // steward 档案 Name=管家
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	sess := mgr.Session("dev1", &fakeTarget{})
	ctx := context.Background()
	if err := sess.WakeSilent(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.HandleText(ctx, "你是谁"); err != nil {
		t.Fatal(err)
	}
	if len(llm.lastMsgs) == 0 {
		t.Fatal("模型没收到任何消息")
	}
	first := llm.lastMsgs[0]
	if first["role"] != "system" {
		t.Fatalf("第一条应是 system，实际 %v", first["role"])
	}
	content := fmt.Sprint(first["content"])
	if !strings.Contains(content, "你的名字是「管家」") || !strings.Contains(content, "我是管家") {
		t.Fatalf("system 里没有显示名称: %q", content)
	}
	if !strings.Contains(content, "你是家庭管家") {
		t.Fatalf("自定义提示词被丢掉了: %q", content)
	}
}
