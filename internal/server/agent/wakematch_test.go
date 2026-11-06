package agent

import "testing"

func TestMatchWakeExactAndSubstring(t *testing.T) {
	words := []string{"小黑", "你好小黑"}
	cases := map[string]bool{
		"小黑":       true,
		"你好小黑":     true,
		"你好小黑。":    true,
		"现在几点":     false,
		"今天的天气怎么样": false,
	}
	for text, want := range cases {
		if _, ok := MatchWake(text, words); ok != want {
			t.Errorf("MatchWake(%q) = %v，期望 %v", text, ok, want)
		}
	}
}

// 两个字一律不容错：2 字容 1 错等于"认对一半就算"。
// 事故现场：喊「小爱同学」被识别成「小同学」，滑窗里"小同"与"小黑"距离 1，
// 直接把小黑唤醒了。宁可多喊一遍，也不能被别的词唤醒。
func TestMatchWakeRejectsTwoCharFuzzy(t *testing.T) {
	words := []string{"小黑"}
	for _, text := range []string{"小飞", "小嘿", "小同学", "小爱同学", "你好，小飞小飞。"} {
		if _, ok := MatchWake(text, words); ok {
			t.Errorf("%q 不该因为容错而匹配到「小黑」", text)
		}
	}
	// 精确说出来的还是要中
	if _, ok := MatchWake("小黑", words); !ok {
		t.Error("精确喊「小黑」应命中")
	}
}

// 不该把完全无关的话当唤醒。
func TestMatchWakeRejectsUnrelated(t *testing.T) {
	words := []string{"小黑"}
	for _, text := range []string{"今天天气不错", "帮我开灯", "小", "黑"} {
		if _, ok := MatchWake(text, words); ok {
			t.Errorf("%q 不该被判为唤醒", text)
		}
	}
}

func TestMatchWakeLongWordTolerance(t *testing.T) {
	words := []string{"你好小黑"}
	if _, ok := MatchWake("你好小飞", words); !ok {
		t.Fatal("四字唤醒词错一个字应命中")
	}
}

// 一句话里既喊唤醒词又提问时，要把剩下的那句拿出来。
func TestMatchWakeExExtractsQuestion(t *testing.T) {
	words := []string{"小黑", "你好小黑"}
	cases := []struct{ text, rest string }{
		{"你好小黑，现在几点", "现在几点"},
		{"小黑 帮我开客厅的灯", "帮我开客厅的灯"},
		{"你好小黑", ""},
		{"你好小飞，今天天气怎么样", "今天天气怎么样"}, // 四字词容 1 错 + 提问题
	}
	for _, c := range cases {
		_, rest, ok := MatchWakeEx(c.text, words)
		if !ok {
			t.Errorf("%q 应命中唤醒词", c.text)
			continue
		}
		if rest != c.rest {
			t.Errorf("%q 的剩余提问 = %q，期望 %q", c.text, rest, c.rest)
		}
	}
}

func TestWakeWordsCollectsAllProfiles(t *testing.T) {
	r, err := NewRegistry([]*Profile{
		{ID: "a", WakeWords: []string{"小黑", "你好小黑"}, LLM: LLM{BaseURL: "u", Model: "m"}},
		{ID: "b", WakeWords: []string{"小智"}, LLM: LLM{BaseURL: "u", Model: "m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.WakeWords()
	if len(got) != 3 {
		t.Fatalf("应收集到 3 个唤醒词，实际 %v", got)
	}
	// 空白词要被丢掉
	r2, err := NewRegistry([]*Profile{
		{ID: "a", WakeWords: []string{"小黑", "  "}, LLM: LLM{BaseURL: "u", Model: "m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r2.WakeWords()); n != 1 {
		t.Fatalf("空白词应被去掉，实际 %v", r2.WakeWords())
	}
}
