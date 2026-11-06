package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// closableEngine 只关心两件事：谁还拿着我、我被关了几次。
// 真实引擎（sherpa）Close 之后再被调用就是访问已释放的 C 对象，
// 这里用 closed 标记把这个错误变成可断言的东西。
type closableEngine struct {
	name string

	mu     sync.Mutex
	closes int
	closed bool
	used   int
}

func (e *closableEngine) SpeechProb([]byte) (float32, error) { return 0, nil }
func (e *closableEngine) Transcribe(context.Context, []byte) (string, error) {
	e.use()
	return "", nil
}
func (e *closableEngine) Synthesize(context.Context, string) ([]byte, error) {
	e.use()
	return nil, nil
}
func (e *closableEngine) TTSRate() int                         { return 16000 }
func (e *closableEngine) DetectKeyword([]byte) (string, error) { return "", nil }
func (e *closableEngine) Status() string                       { return e.name }
func (e *closableEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closes++
	e.closed = true
	return nil
}

func (e *closableEngine) use() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.used++
}

func (e *closableEngine) state() (closes, used int, closed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closes, e.used, e.closed
}

func TestEngineHandle_RetireWaitsForUsers(t *testing.T) {
	old := &closableEngine{name: "old"}
	h := newEngineHandle(old)

	// 借两份（模拟对话框 + 设备管道）
	eng, ok := h.acquire()
	if !ok || eng != old {
		t.Fatalf("acquire 失败: ok=%v eng=%v", ok, eng)
	}
	if _, ok := h.acquire(); !ok {
		t.Fatal("第二次 acquire 失败")
	}
	h.refsCheck(t, 3)

	// 换引擎：服务端放手 + 退役，但还有两个使用者
	h.retire()
	if closes, _, _ := old.state(); closes != 0 {
		t.Fatalf("还有使用者时不该 Close，实际 Close 了 %d 次", closes)
	}
	if _, ok := h.acquire(); ok {
		t.Fatal("已退休的句柄不该还能借到引用")
	}

	h.release()
	if closes, _, _ := old.state(); closes != 0 {
		t.Fatalf("还剩一个使用者时不该 Close，实际 Close 了 %d 次", closes)
	}
	h.release()
	if closes, _, _ := old.state(); closes != 1 {
		t.Fatalf("最后一个使用者放手后应当 Close 一次，实际 %d 次", closes)
	}

	// 重复 release 不能把计数打成负数、也不能二次 Close
	h.release()
	if closes, _, _ := old.state(); closes != 1 {
		t.Fatalf("重复 release 导致重复 Close：%d", closes)
	}
}

// refsCheck 直接读内部计数做断言（同包测试，允许）。
func (h *engineHandle) refsCheck(t *testing.T, want int) {
	t.Helper()
	h.mu.Lock()
	got := h.refs
	h.mu.Unlock()
	if got != want {
		t.Fatalf("引用数 = %d，期望 %d", got, want)
	}
}

func TestServerInstallEngine_ClosesOldOnlyAfterPipelineStops(t *testing.T) {
	s := &server{devAudio: make(map[string]*deviceAudio)}
	old := &closableEngine{name: "old"}
	s.installEngine(old)

	// 模拟一条设备管道借走引擎
	eng, release, ok := s.acquireEngine()
	if !ok || eng != old {
		t.Fatalf("acquireEngine 失败")
	}

	// 换新引擎：旧的不该立刻关，管道还在用
	cur := &closableEngine{name: "new"}
	s.installEngine(cur)
	if closes, _, _ := old.state(); closes != 0 {
		t.Fatalf("管道还在用旧引擎时不该 Close，实际 %d 次", closes)
	}
	if got := s.getEngine(); got != cur {
		t.Fatalf("当前引擎应是 new，实际 %v", got)
	}

	release() // 管道退出
	if closes, _, _ := old.state(); closes != 1 {
		t.Fatalf("管道放手后旧引擎应 Close 一次，实际 %d 次", closes)
	}
	if closes, _, _ := cur.state(); closes != 0 {
		t.Fatalf("新引擎不该被关")
	}
}

// 设备管道必须能被真正停掉：旧实现只把它从 map 里删掉，
// 协程永远挂在 context.Background() 上，改一次配置泄漏一条。
func TestDeviceAudioStopStopsGoroutineAndReleasesEngine(t *testing.T) {
	eng := &closableEngine{name: "e"}
	h := newEngineHandle(eng)
	e, ok := h.acquire()
	if !ok {
		t.Fatal("acquire 失败")
	}
	d := newDeviceAudio("dev1", nil, e, h.release, nil, deviceOptions{
		MinSpeech: 100 * time.Millisecond, MinSilence: 100 * time.Millisecond,
	}, nil, nil, nil, nil, nil)

	d.start(context.Background())
	d.stop(time.Second)

	select {
	case <-d.doneCh:
	default:
		t.Fatal("stop 返回后消费协程应已退出")
	}
	// 服务端那份 + 管道那份都还回来之后才 Close
	h.retire()
	if closes, _, _ := eng.state(); closes != 1 {
		t.Fatalf("引用还清后应 Close 一次，实际 %d 次", closes)
	}
}

// 推一帧到已停止的管道不能 panic，也不能复活协程。
func TestDeviceAudioPushAfterStopIsSafe(t *testing.T) {
	d := newDeviceAudio("dev2", nil, nil, nil, nil, deviceOptions{}, nil, nil, nil, nil, nil)
	d.start(context.Background())
	d.stop(time.Second)
	d.Push(make([]byte, 1024))
}
