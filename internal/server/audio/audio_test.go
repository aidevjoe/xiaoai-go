package audio

import "testing"

func frame(b byte) []byte {
	f := make([]byte, BytesPerFrame)
	for i := range f {
		f[i] = b
	}
	return f
}

func TestSegmenterIgnoresShortNoise(t *testing.T) {
	s := NewSegmenter()
	var out []byte
	// 100ms 的短促声音（低于 250ms 阈值）
	for i := 0; i < 3; i++ {
		out = s.Push(frame(1), true)
	}
	if out != nil {
		t.Fatal("短促噪声不应触发分段")
	}
	if s.Speaking() {
		t.Fatal("不应进入说话状态")
	}
}

func TestSegmenterEmitsAfterSilence(t *testing.T) {
	s := NewSegmenter()
	var got []byte
	// 300ms 语音（≥250ms 阈值）
	for i := 0; i < 10; i++ {
		if out := s.Push(frame(1), true); out != nil {
			t.Fatalf("说话过程中不应提前输出（第 %d 帧）", i)
		}
	}
	if !s.Speaking() {
		t.Fatal("应处于说话状态")
	}
	// 400ms 静音（≥300ms 阈值）
	for i := 0; i < 13; i++ {
		if out := s.Push(frame(0), false); out != nil {
			got = out
			break
		}
	}
	if got == nil {
		t.Fatal("静音后应输出一句话")
	}
	// 语音 10 帧 + 至少部分静音帧
	if len(got) < 10*BytesPerFrame {
		t.Fatalf("输出过短: %d 字节", len(got))
	}
	if s.Speaking() {
		t.Fatal("输出后应回到空闲")
	}
}

func TestSegmenterIncludesPreRoll(t *testing.T) {
	s := NewSegmenter()
	// 先来 200ms 非语音（会被放进预缓冲）
	for i := 0; i < 6; i++ {
		if out := s.Push(frame(9), false); out != nil {
			t.Fatal("静音期不应输出")
		}
	}
	// 再说 300ms
	for i := 0; i < 10; i++ {
		s.Push(frame(1), true)
	}
	// 静音收尾
	var got []byte
	for i := 0; i < 13; i++ {
		if out := s.Push(frame(0), false); out != nil {
			got = out
			break
		}
	}
	if got == nil {
		t.Fatal("应输出一句话")
	}
	// 开头应包含预缓冲的内容（值为 9 的帧）
	if got[0] != 9 {
		t.Fatalf("预缓冲未拼接：首字节为 %d", got[0])
	}
}

func TestSegmenterCapsMaxUtterance(t *testing.T) {
	// 上限设短一点便于测试
	s := NewSegmenterWith(100*1000*1000, // 100ms
		300*1000*1000, // 300ms
		0,
		500*1000*1000) // 500ms
	var out []byte
	for i := 0; i < 40; i++ { // 40 × 32ms ≈ 1.28s，远超 500ms
		if o := s.Push(frame(1), true); o != nil {
			out = o
			break
		}
	}
	if out == nil {
		t.Fatal("超过上限应强制切断")
	}
	maxBytes := int(0.5 * SampleRate * 2)
	if len(out) > maxBytes+BytesPerFrame {
		t.Fatalf("输出 %d 字节超过上限 %d", len(out), maxBytes)
	}
}

func TestSegmenterReset(t *testing.T) {
	s := NewSegmenter()
	for i := 0; i < 10; i++ {
		s.Push(frame(1), true)
	}
	if s.Pending() == 0 {
		t.Fatal("应累积了音频")
	}
	s.Reset()
	if s.Pending() != 0 || s.Speaking() {
		t.Fatal("Reset 后应清空")
	}
}

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"两句", "你好。今天天气怎么样？", []string{"你好。", "今天天气怎么样？"}},
		{"连续问号", "真的吗？！我不信。", []string{"真的吗？！", "我不信。"}},
		{"无标点长句", "我我我我我我我我我我我我我我我我我我我我我我我我", []string{"我我我我我我我我我我我我我我我我我我我我", "我我我我"}},
		{"逗号优先", "今天外面的天气真的很不错，我们出去走一走吧", []string{"今天外面的天气真的很不错，", "我们出去走一走吧"}},
		{"空字符串", "   ", nil},
		{"英文标点", "Hello there. How are you?", []string{"Hello there.", "How are you?"}},
	}
	for _, c := range cases {
		got := SplitSentences(c.in, 20)
		if len(got) != len(c.want) {
			t.Errorf("%s: 期望 %d 段，实际 %d 段: %#v", c.name, len(c.want), len(got), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: 第 %d 段期望 %q，实际 %q", c.name, i, c.want[i], got[i])
			}
		}
	}
}

func TestSplitSentencesRespectsLimit(t *testing.T) {
	long := "这是一个很长的句子用来测试切分逻辑是否会在超长时退化到逗号处切分并保证每段都不超过限制"
	parts := SplitSentences(long, 20)
	if len(parts) < 2 {
		t.Fatalf("超长文本应被切开，实际 %d 段", len(parts))
	}
	for _, p := range parts {
		if n := len([]rune(p)); n > 20 {
			t.Fatalf("片段 %q 长度 %d 超过上限 20", p, n)
		}
	}
	// 拼接后应还原原文（去掉切分产生的空白）
	joined := ""
	for _, p := range parts {
		joined += p
	}
	if joined != long {
		t.Fatalf("切分丢失内容:\n原文 %q\n拼接 %q", long, joined)
	}
}

func TestSanitizeStripsEmojiAndMarkdown(t *testing.T) {
	cases := map[string]string{
		"**客厅灯**已打开":   "客厅灯已打开",
		"好的👍":          "好的",
		"今天 天气 不错":     "今天 天气 不错",
		"`code` 和 #标题": "code 和 标题",
		"☀️晴天":         "晴天",
		"正常一句话。":       "正常一句话。",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestEncodeWAVHeader(t *testing.T) {
	pcm := make([]byte, 3200)
	w := EncodeWAV(pcm, 16000)
	if string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" || string(w[36:40]) != "data" {
		t.Fatalf("WAV 头不对: %q", w[:44])
	}
	if len(w) != 44+len(pcm) {
		t.Fatalf("长度不对: %d", len(w))
	}
	if got := int(uint32(w[24]) | uint32(w[25])<<8 | uint32(w[26])<<16 | uint32(w[27])<<24); got != 16000 {
		t.Fatalf("采样率写入错误: %d", got)
	}
}
