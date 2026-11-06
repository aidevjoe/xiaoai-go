package dialog

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"xiaoai-go/internal/server/audio"
)

// fakeTextTarget 记录"设备自己念"的文字，用来断言设备原生 TTS 路径。
// 报错不动（SayText 是唯一的发声路径），所以不需要 PlayPCM。
type fakeTextTarget struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeTextTarget) SayText(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return nil
}

// PlayPCM 是 Target 接口要求；DeviceTTS 模式下不会走到这里。
func (f *fakeTextTarget) PlayPCM(ctx context.Context, pcm []byte, rate int) error {
	return nil
}

func (f *fakeTextTarget) said() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// 回归：等待模型首字的这段时间不再插入任何"我查一下，稍等"之类的占位语音。
// 旧实现会在超过阈值时先念一句提示、真答案到了再掐断；该功能已整体移除，
// 这里既守住"等待期间不出声"，也守住"别把提示语混进答案里"。
func TestNoWaitingHintWhenModelIsSlow(t *testing.T) {
	llm := newFakeLLM(t, "慢回答来了。")
	llm.Delay = 350 * time.Millisecond
	reg := testRegistry(t, llm.URL, llm.URL)
	eng := audio.NewMockEngine(0.9, "x")
	mgr := NewManager(reg, nil, eng, log.New(io.Discard, "", 0))
	mgr.Speak.DeviceTTS = true

	tgt := &fakeTextTarget{}
	sess := mgr.Session("dev1", tgt)
	ctx := context.Background()
	if err := sess.Wake(ctx, "小黑"); err != nil {
		t.Fatal(err)
	}
	before := len(tgt.said()) // 唤醒应答本身不算等待提示

	errCh := make(chan error, 1)
	go func() {
		_, err := sess.HandleText(ctx, "查一下天气")
		errCh <- err
	}()

	// 模型还在"想"（Delay 350ms），这段窗口里设备不该再多说一个字
	time.Sleep(150 * time.Millisecond)
	if got := len(tgt.said()); got != before {
		t.Fatalf("等待模型期间不该播占位语音，实际念了：%v", tgt.said())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // 若还残留延迟计时器，也该醒了

	joined := strings.Join(tgt.said(), "|")
	if strings.Contains(joined, "我查一下") || strings.Contains(joined, "稍等") {
		t.Fatalf("等待提示语仍被播报：%v", tgt.said())
	}
	if !strings.Contains(joined, "慢回答来了") {
		t.Fatalf("真正的回答没有被播出来：%v", tgt.said())
	}
}
