package dialog

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingTextTarget 记录设备原生 TTS 真正收到的每一段文本。
type recordingTextTarget struct {
	mu   sync.Mutex
	said []string
}

func (t *recordingTextTarget) PlayPCM(context.Context, []byte, int) error { return nil }
func (t *recordingTextTarget) SayText(_ context.Context, text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.said = append(t.said, text)
	return nil
}
func (t *recordingTextTarget) texts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.said...)
}

func newDeviceSpeaker(tgt Target) *StreamSpeaker {
	return NewStreamSpeakerWith(context.Background(), nil, tgt, SpeakOptions{
		MaxSentenceRunes: 20, MaxSentences: 0, StripEmoji: true, DeviceTTS: true,
	})
}

// 实测事故（设备日志 11:33:01）：
//
//	收到播报请求: 香蕉的英文还是 bana
//	收到播报请求: na。
//
// 首句 12 字硬切把 banana 劈成两半，设备把两段各自合成、各自起播放器，
// 听起来是 "banna … a"，中间还空一截。短回答必须整段一次下发。
func TestDeviceTTS_ShortAnswerIsOneUtterance(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"香蕉的英文还是 banana", "banana"},
		{"苹果的英文是 apple", "apple"},
		{"气温是 3.14 度", "3.14"},
		{"可以说 apple pie", "apple pie"},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			tgt := &recordingTextTarget{}
			sp := newDeviceSpeaker(tgt)
			// 还原真实流式：正文先到，句号单独作为一个增量
			sp.Write(c.body)
			sp.Write("。")
			if err := sp.Close(); err != nil {
				t.Fatal(err)
			}
			got := tgt.texts()
			if len(got) != 1 {
				t.Fatalf("短回答应一次下发，实际 %d 段: %q", len(got), got)
			}
			if !strings.Contains(got[0], c.want) {
				t.Fatalf("下发内容不完整/单词被拆: %q，期望包含 %q", got[0], c.want)
			}
			// 半个单词结尾是最典型的坏味道
			for _, half := range []string{"bana", "banan", "appl", "3."} {
				if strings.HasSuffix(strings.TrimSuffix(got[0], "。"), half) {
					t.Fatalf("下发停在半个词上 %q: %q", half, got[0])
				}
			}
		})
	}
}

func TestSplitFirst_NeverCutsInsideLatinWord(t *testing.T) {
	cut, tail := splitFirst("香蕉的英文还是 banana", 12)
	if cut == "" || tail == "" {
		t.Fatalf("应该切出前缀与剩余，得到 %q / %q", cut, tail)
	}
	if strings.HasSuffix(cut, "bana") || strings.HasSuffix(cut, "banan") {
		t.Fatalf("切点落在 banana 中间: %q / %q", cut, tail)
	}
	if cut+tail != "香蕉的英文还是 banana" {
		t.Fatalf("切分后文本应无损（含词间空格）: %q / %q", cut, tail)
	}
}

// 小数点不能当句末标点，否则 "3.14" 会被切成 "3." 和 "14"。
func TestSplitFirst_DecimalPointIsNotBoundary(t *testing.T) {
	cut, tail := splitFirst("气温是 3.14 度。", 40)
	if cut != "气温是 3.14 度。" || tail != "" {
		t.Fatalf("小数点被当成断句点了: %q / %q", cut, tail)
	}
}

// 单独一个句号不该成为一段独立语音（设备会为它生成一段自带静音的音频）。
func TestEmit_SkipsPunctuationOnlySegment(t *testing.T) {
	tgt := &recordingTextTarget{}
	sp := newDeviceSpeaker(tgt)
	sp.emit("好的")
	sp.emit("。") // 模型把句号单独作为一个增量吐出来
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	got := tgt.texts()
	if len(got) != 1 || got[0] != "好的" {
		t.Fatalf("纯标点段应被跳过，实际 %q", got)
	}
}

// 句号跟在正文后面到达时应并入同一句，不另起一段。
func TestDeviceTTS_PunctuationJoinsPreviousSentence(t *testing.T) {
	tgt := &recordingTextTarget{}
	sp := newDeviceSpeaker(tgt)
	sp.Write("好的")
	sp.Write("。")
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	got := tgt.texts()
	if len(got) != 1 || got[0] != "好的。" {
		t.Fatalf("应只下发一次完整句子，实际 %q", got)
	}
}

// 实测事故（设备日志 12:27:14）：
//
//	收到播报请求: 香蕉的英文是 banana，
//	收到播报请求: 复数就是 bananas。
//
// 两段在播放侧是首尾相接的（念完时间 17.47 / 17.44），但云端给每段
// 独立合成：段尾逗号被念成句末长停顿，下一段又自带前导静音——
// 用户听到"逗号之后停了两秒"。首段攒够后稍等 400ms，整句一段下发。
func TestDeviceTTS_CommaAnswerIsOneUtterance(t *testing.T) {
	tgt := &recordingTextTarget{}
	sp := newDeviceSpeaker(tgt)
	sp.Write("香蕉的英文是 banana，") // 首句在逗号处被切出，14 字 ≥ firstChunkRunes
	sp.Write("复数就是 bananas。")  // 112ms 后第二句就到了，应该并进同一段
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	got := tgt.texts()
	if len(got) != 1 {
		t.Fatalf("逗号句应并成一段下发，实际 %d 段: %q", len(got), got)
	}
	if got[0] != "香蕉的英文是 banana，复数就是 bananas。" {
		t.Fatalf("下发内容不对: %q", got[0])
	}
}

// 长回答不能等 Close 才开口：攒满 maxPendingRunes 就得先发第一段。
// 注意要按流式节奏一小段一小段 Write——一次性 Write 整段时 splitFirst
// 会从最近的逗号整段切出，本来就只有一段。
func TestDeviceTTS_LongAnswerSendsFirstChunkWithoutWaiting(t *testing.T) {
	tgt := &recordingTextTarget{}
	sp := newDeviceSpeaker(tgt)
	for i := 0; i < 6; i++ {
		sp.Write("这是一句比较长的话，")
		time.Sleep(20 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(tgt.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(tgt.texts()) == 0 {
		t.Fatal("长回答应攒满即发第一段，不该等 Close")
	}
	first := tgt.texts()[0]
	if got := len([]rune(first)); got > 2*maxPendingRunes {
		t.Fatalf("第一段不该把整轮都带上（%d 字）: %q", got, first)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if got := tgt.texts(); len(got) < 2 {
		t.Fatalf("长回答应有后续段落，实际 %q", got)
	}
}
