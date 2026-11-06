package dialog

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"xiaoai-go/internal/server/audio"
)

// 实测事故（设备日志 12:26~12:27）：播报结束后混响没散，麦克风把
// 自己刚说的最后几个字又识别了一遍——"尽管说""您说"被当成提问，
// 模型接着自言自语循环。识别文本是最近某句播报的结尾时要当没听见。
func TestHandleText_IgnoresEchoOfOwnReplyTail(t *testing.T) {
	llm := newFakeLLM(t, "有什么要我帮忙的，尽管说。")
	reg := testRegistry(t, llm.URL, llm.URL)
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	sess := mgr.Session("dev1", &fakeTarget{})
	ctx := context.Background()
	if err := sess.WakeSilent(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.HandleText(ctx, "在吗"); err != nil {
		t.Fatal(err)
	}
	calls := len(llm.lastMsgs)

	// 混响里听到的尾巴
	turn, err := sess.HandleText(ctx, "尽管说")
	if err != nil {
		t.Fatal(err)
	}
	if turn != nil {
		t.Fatalf("回声应被忽略，实际当成提问处理了: %+v", turn)
	}
	if len(llm.lastMsgs) != calls {
		t.Fatalf("回声不该再打到模型，调用数 %d → %d", calls, len(llm.lastMsgs))
	}

	// 正常提问不受影响
	if _, err := sess.HandleText(ctx, "今天天气怎么样"); err != nil {
		t.Fatal(err)
	}
	if len(llm.lastMsgs) <= calls {
		t.Fatal("正常提问被误判成回声")
	}
}

// 唤醒应答"在呢"被听到时也不该触发一轮对话。
func TestIsEcho_WakeReplyTail(t *testing.T) {
	llm := newFakeLLM(t, "好的。")
	reg := testRegistry(t, llm.URL, llm.URL)
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	sess := mgr.Session("dev1", &fakeTarget{})
	if err := sess.Wake(context.Background(), "小黑"); err != nil {
		t.Fatal(err)
	}
	if !sess.IsEcho("在呢") {
		t.Fatal("唤醒应答的尾巴应被识别为回声")
	}
	if sess.IsEcho("打开客厅的灯") {
		t.Fatal("正常指令不该被当成回声")
	}
}

// 太久之前说的话不用再比：隔了半天用户真说"尽管说"也得照常回答。
func TestIsEcho_ExpiresAfterWindow(t *testing.T) {
	llm := newFakeLLM(t, "好的。")
	reg := testRegistry(t, llm.URL, llm.URL)
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	sess := mgr.Session("dev1", &fakeTarget{})
	if err := sess.Wake(context.Background(), "小黑"); err != nil {
		t.Fatal(err)
	}
	sess.mu.Lock()
	for i := range sess.spoken {
		sess.spoken[i].at = time.Now().Add(-2 * echoWindow)
	}
	sess.mu.Unlock()
	if sess.IsEcho("在呢") {
		t.Fatal("超过比对窗口的旧话不该再算回声")
	}
}
