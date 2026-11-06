package audio

import (
	"context"
	"testing"
)

// TestPipelineEmitsText 验证：静音不进 ASR，一句话结束后只识别一次。
func TestPipelineEmitsText(t *testing.T) {
	eng := NewMockEngine(0.9, "打开书房的灯")
	// 全零帧视为静音，其余视为人声
	eng.ProbFunc = func(f []byte) float32 {
		for _, b := range f {
			if b != 0 {
				return 0.9
			}
		}
		return 0.02
	}
	var gotText string
	var gotLen int
	p := NewPipeline(eng, func(text string, pcm []byte) {
		gotText = text
		gotLen = len(pcm)
	})

	ctx := context.Background()
	// 300ms 语音
	for i := 0; i < 10; i++ {
		if _, err := p.Push(ctx, frame(1)); err != nil {
			t.Fatal(err)
		}
	}
	if eng.CallCount != 0 {
		t.Fatal("句子没结束就不该调用 ASR")
	}
	if !p.Speaking() {
		t.Fatal("应处于说话状态")
	}

	// 400ms 静音触发结束
	for i := 0; i < 13; i++ {
		if _, err := p.Push(ctx, frame(0)); err != nil {
			t.Fatal(err)
		}
	}
	if eng.CallCount != 1 {
		t.Fatalf("应只识别一次，实际 %d 次", eng.CallCount)
	}
	if gotText != "打开书房的灯" {
		t.Fatalf("识别文本不匹配: %q", gotText)
	}
	if gotLen == 0 {
		t.Fatal("回调应带上原始音频")
	}
	if p.Speaking() {
		t.Fatal("一句话结束后应回到空闲")
	}
}

// TestPipelineSilenceOnly 验证：一直静音不会调用 ASR。
func TestPipelineSilenceOnly(t *testing.T) {
	eng := NewMockEngine(0.0, "不该出现")
	p := NewPipeline(eng, func(string, []byte) {
		t.Fatal("静音不应触发识别")
	})
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := p.Push(ctx, frame(0)); err != nil {
			t.Fatal(err)
		}
	}
	if eng.CallCount != 0 {
		t.Fatal("静音期间不应调用 ASR")
	}
}

// TestPipelineRespectsVAD 验证：VAD 判定为非人声的帧不会被当成语音。
func TestPipelineRespectsVAD(t *testing.T) {
	eng := NewMockEngine(0.2, "不该出现") // 低于 0.5 阈值
	p := NewPipeline(eng, func(string, []byte) {
		t.Fatal("VAD 判为非语音时不应触发识别")
	})
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := p.Push(ctx, frame(1)); err != nil {
			t.Fatal(err)
		}
	}
	if eng.CallCount != 0 {
		t.Fatal("不应调用 ASR")
	}
}

// TestPipelineReset 验证打断后不残留。
func TestPipelineReset(t *testing.T) {
	eng := NewMockEngine(0.9, "x")
	eng.ProbFunc = func(f []byte) float32 { return 0.9 }
	p := NewPipeline(eng, func(string, []byte) {})
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		p.Push(ctx, frame(1))
	}
	if !p.Speaking() {
		t.Fatal("应在说话状态")
	}
	p.Reset()
	if p.Speaking() {
		t.Fatal("Reset 后应清空")
	}
}
