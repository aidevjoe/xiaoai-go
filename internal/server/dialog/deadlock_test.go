package dialog

import (
	"context"
	"testing"
	"time"

	"xiaoai-go/internal/server/audio"
)

// 这次线上事故的回归测试：
// SayWakeReply / SayExitReply 在持有 s.mu 时又调 speakOptions()（内部也要 s.mu），
// Go 的 Mutex 不可重入 → 协程永久卡住并一直占着 s.mu。
// 接着 Sweep 拿着 m.mu 等 s.mu，后台保存的 SetRegistry 等 m.mu，
// 表现就是"点完成显示已保存，但右上角永远保存中，刷新还是旧值"。

func mustFinish(t *testing.T, name string, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s 在 %v 内没有返回：锁被永久占住了", name, timeout)
	}
}

func TestSayRepliesDoNotSelfDeadlock(t *testing.T) {
	mgr, tgt := quietManager(t, "好的。")
	sess := mgr.Session("dev", tgt)
	if err := sess.Wake(context.Background(), "小黑"); err != nil {
		t.Fatalf("唤醒失败: %v", err)
	}

	mustFinish(t, "SayWakeReply", 3*time.Second, func() {
		sess.SayWakeReply(context.Background())
	})
	mustFinish(t, "SayExitReply", 3*time.Second, func() {
		sess.SayExitReply(context.Background())
	})

	// 会话锁必须能被别人拿到（Sweep 会拿它，后台保存会拿 Manager 的锁）
	mustFinish(t, "Session.State", 2*time.Second, func() { sess.State() })
	mustFinish(t, "Session.idleTimeout", 2*time.Second, func() { sess.idleTimeout() })
	mustFinish(t, "Manager.SetRegistry", 2*time.Second, func() {
		mgr.SetRegistry(mgr.Reg)
	})
	mustFinish(t, "Manager.SetEngine", 2*time.Second, func() {
		mgr.SetEngine(audio.NewMockEngine(0.9, "x"))
	})
}

// Sweep 必须先在 m.mu 下拷贝会话指针、放锁后再读每个会话的锁，
// 否则"拿着 m.mu 等 s.mu"会和任何"拿着 s.mu 等 m.mu"的路径形成锁序反转。
func TestSweepDoesNotHoldManagerLockWhileReadingSessions(t *testing.T) {
	mgr, tgt := quietManager(t, "好的。")
	sess := mgr.Session("dev", tgt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Sweep(ctx, 10*time.Millisecond)

	// 给 Sweep 几个 tick 的机会真正跑起来
	time.Sleep(60 * time.Millisecond)
	mustFinish(t, "Sweep 期间 SetRegistry", 2*time.Second, func() {
		mgr.SetRegistry(mgr.Reg)
	})
	mustFinish(t, "Sweep 期间会话状态", 2*time.Second, func() {
		sess.State()
	})
}
