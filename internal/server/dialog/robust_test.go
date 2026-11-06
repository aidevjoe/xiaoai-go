package dialog

import (
	"context"
	"io"
	"log"
	"runtime"
	"sync"
	"testing"
	"time"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
)

func quietManager(t *testing.T, reply string) (*Manager, *fakeTarget) {
	t.Helper()
	llmSrv := newFakeLLM(t, reply)
	reg := testRegistry(t, llmSrv.URL, llmSrv.URL)
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	tgt := &fakeTarget{}
	return mgr, tgt
}

// 旧 bridge 的毛病之一：状态卡住后再也不响应。
// 我们要求：未唤醒时 Active()=false，唤醒后为 true，一轮结束后回到 true 可继续。
func TestActiveOnlyAfterWake(t *testing.T) {
	mgr, tgt := quietManager(t, "好的。")
	sess := mgr.Session("dev", tgt)
	ctx := context.Background()

	if sess.Active() {
		t.Fatal("未唤醒时不该是可响应状态")
	}
	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	if !sess.Active() {
		t.Fatal("唤醒后应可响应")
	}
	if _, err := sess.HandleText(ctx, "你好"); err != nil {
		t.Fatal(err)
	}
	// 一轮结束后仍在聆听（连续对话），用户可以继续说
	if !sess.Active() {
		t.Fatalf("一轮结束后应继续聆听，实际状态 %s", sess.State())
	}
}

// 静音超时要回到 idle，否则设备会被"永久占用"（旧 bug 的另一种形态）。
func TestSweepIdlesExpiredSession(t *testing.T) {
	mgr, tgt := quietManager(t, "好的。")
	mgr.Timeout = 50 * time.Millisecond
	sess := mgr.Session("dev", tgt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Sweep(ctx, 20*time.Millisecond)

	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	// 唤醒应答会刷新活跃时间，等它超时
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sess.State() == StateIdle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("超时后应回到 idle，实际 %s", sess.State())
}

// 超时后必须能重新唤醒——不能"叫一次就死"。
func TestWakeAgainAfterIdle(t *testing.T) {
	mgr, tgt := quietManager(t, "好的。")
	sess := mgr.Session("dev", tgt)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := sess.Wake(ctx, "小黑"); err != nil {
			t.Fatalf("第 %d 次唤醒失败: %v", i+1, err)
		}
		if _, err := sess.HandleText(ctx, "再说一句"); err != nil {
			t.Fatalf("第 %d 轮失败: %v", i+1, err)
		}
		sess.Idle()
	}
}

// 并发调用同一会话不能崩、不能串话（配合 -race 跑）。
func TestConcurrentTurnsAreSerialized(t *testing.T) {
	mgr, tgt := quietManager(t, "好。")
	sess := mgr.Session("dev", tgt)
	ctx := context.Background()
	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = sess.HandleText(ctx, "并发问题")
		}()
	}
	wg.Wait()

	// 历史不该无限增长（上限 20 条）
	sess.mu.Lock()
	n := len(sess.history)
	sess.mu.Unlock()
	if n > 20 {
		t.Fatalf("历史超过上限: %d", n)
	}
}

// 换档案必须清空上下文，避免把管家看到的设备状态带给闲聊助手。
func TestHistoryClearedOnProfileSwitch(t *testing.T) {
	llmA := newFakeLLM(t, "我是管家。")
	llmB := newFakeLLM(t, "我是闲聊。")
	reg := testRegistry(t, llmA.URL, llmB.URL)
	mgr := NewManager(reg, nil, audio.NewMockEngine(0.9, "x"), log.New(io.Discard, "", 0))
	sess := mgr.Session("dev", &fakeTarget{})
	ctx := context.Background()

	sess.Wake(ctx, "小黑")
	sess.HandleText(ctx, "打开灯")

	sess.Wake(ctx, "小智")
	sess.HandleText(ctx, "你好")

	// 第二轮请求应只有 system + user
	if len(llmB.lastMsgs) != 2 {
		t.Fatalf("换档案后历史未清空，消息数 %d", len(llmB.lastMsgs))
	}
}

func TestProfileExitWords(t *testing.T) {
	p := &agent.Profile{ID: "x", WakeWords: []string{"a"}, LLM: agent.LLM{BaseURL: "u", Model: "m"}}
	if p.IsExit("好的") {
		t.Fatal("普通词不该判为退出")
	}
	if !p.IsExit("退下吧") {
		t.Fatal("应识别退出词")
	}
}

// 每轮都会起一个合成协程，必须确保它退出——否则跑一夜就是几万个泄漏协程。
func TestNoGoroutineLeakPerTurn(t *testing.T) {
	mgr, tgt := quietManager(t, "好。")
	sess := mgr.Session("dev", tgt)
	ctx := context.Background()
	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	// 先跑几轮预热，排除首次加载的影响
	for i := 0; i < 3; i++ {
		sess.HandleText(ctx, "预热")
	}
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 30; i++ {
		sess.HandleText(ctx, "测试泄漏")
	}
	time.Sleep(150 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after > before+5 {
		t.Fatalf("协程疑似泄漏：30 轮前 %d，之后 %d", before, after)
	}
}
