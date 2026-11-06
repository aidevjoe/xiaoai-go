package main

import "testing"

// 唤醒应答是"不闭麦"播的，麦克风会把"诶"自己收进来。
// 这类开头的语气词要剥掉，否则会被当成用户说的话送去模型。
func TestTrimLeadingFillers(t *testing.T) {
	cases := map[string]string{
		"诶。":        "",
		"诶，今天天气怎么样": "今天天气怎么样",
		"嗯。帮我开灯":    "帮我开灯",
		"哎，诶，在吗":    "在吗",
		"诶诶":        "",
		"帮我开灯":      "帮我开灯",
		"在客厅的灯打开":   "在客厅的灯打开", // "在"不是语气词，不能剥
	}
	for in, want := range cases {
		if got := trimLeadingFillers(in); got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
}

// 剥完为空时不能把空串送去模型（否则模型会自问自答）。
func TestTrimLeadingFillersEmptyStaysEmpty(t *testing.T) {
	if got := trimLeadingFillers("诶。"); got != "" {
		t.Fatalf("纯语气词应该剥成空串，实际 %q", got)
	}
}
