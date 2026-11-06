package dialog

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"

	"xiaoai-go/internal/server/audio"
)

// countingEngine 统计合成次数，用来证明"第二次不再合成"。
type countingEngine struct {
	audio.Engine
	synth int32
}

func (c *countingEngine) Synthesize(ctx context.Context, text string) ([]byte, error) {
	atomic.AddInt32(&c.synth, 1)
	return c.Engine.Synthesize(ctx, text)
}

func (c *countingEngine) Speed() float32 { return 1 }

func (c *countingEngine) count() int32 { return atomic.LoadInt32(&c.synth) }

func newCacheTestManager(t *testing.T) (*Manager, *countingEngine) {
	t.Helper()
	reg := testRegistry(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	eng := &countingEngine{Engine: audio.NewMockEngine(0.9, "x")}
	mgr := NewManager(reg, nil, eng, log.New(io.Discard, "", 0))
	mgr.Fixed = NewReplyCache(t.TempDir())
	mgr.Speak.DeviceTTS = false
	return mgr, eng
}

// 核心行为：第一次合成，第二次直接播缓存。
func TestReplyCache_SecondCallUsesCache(t *testing.T) {
	mgr, eng := newCacheTestManager(t)
	tgt := &fakeTarget{}
	ctx := context.Background()
	const text = "我在"

	if err := mgr.sayCached(ctx, tgt, text, DefaultSpeakOptions()); err != nil {
		t.Fatal(err)
	}
	if eng.count() != 1 {
		t.Fatalf("首次应合成 1 次，实际 %d", eng.count())
	}
	if err := mgr.sayCached(ctx, tgt, text, DefaultSpeakOptions()); err != nil {
		t.Fatal(err)
	}
	if eng.count() != 1 {
		t.Fatalf("第二次不该再合成，实际合成 %d 次", eng.count())
	}
	if tgt.count() != 2 {
		t.Fatalf("两次都应该播出去，实际播放 %d 次", tgt.count())
	}
}

// 文案改了就必须重新合成（缓存键含文本）。
func TestReplyCache_TextChangeResynthesizes(t *testing.T) {
	mgr, eng := newCacheTestManager(t)
	tgt := &fakeTarget{}
	ctx := context.Background()

	_ = mgr.sayCached(ctx, tgt, "我在", DefaultSpeakOptions())
	_ = mgr.sayCached(ctx, tgt, "我在呢", DefaultSpeakOptions())
	if eng.count() != 2 {
		t.Fatalf("文案变了应重新合成，实际合成 %d 次", eng.count())
	}
}

// 设备原生 TTS 不走缓存：音频在设备侧生成，服务端拿不到字节。
func TestReplyCache_DeviceTTSBypassesCache(t *testing.T) {
	mgr, eng := newCacheTestManager(t)
	tgt := &fakeTextTarget{}
	ctx := context.Background()
	opt := DefaultSpeakOptions()
	opt.DeviceTTS = true

	_ = mgr.sayCached(ctx, tgt, "我在", opt)
	_ = mgr.sayCached(ctx, tgt, "我在", opt)
	if eng.count() != 0 {
		t.Fatalf("设备模式不该本地合成，实际合成 %d 次", eng.count())
	}
	if len(tgt.said()) != 2 {
		t.Fatalf("应该交给设备念两次，实际 %v", tgt.said())
	}
}

// 过长的文本不进缓存（固定应答都是短句，长句缓存只会白占内存）。
func TestReplyCache_LongTextBypassesCache(t *testing.T) {
	mgr, eng := newCacheTestManager(t)
	tgt := &fakeTarget{}
	ctx := context.Background()
	long := "这是一段明显超过固定应答长度上限的文本用来验证它不会被塞进缓存里面去占用内存空间所以要多写几个字"
	if runeCount(long) <= replyCacheMaxRunes {
		t.Fatalf("测试文本不够长（%d 字）", runeCount(long))
	}
	_ = mgr.sayCached(ctx, tgt, long, DefaultSpeakOptions())
	first := eng.count()
	_ = mgr.sayCached(ctx, tgt, long, DefaultSpeakOptions())
	// 长文本每次都走正常合成；而正常路径会按句拆开，所以次数是"句数 × 调用次数"。
	// 这里只关心"第二次又合成了"。
	if eng.count() == first {
		t.Fatalf("长文本不该命中缓存，合成次数没变（%d）", first)
	}
}
