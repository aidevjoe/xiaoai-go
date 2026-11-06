package main

import "testing"

func TestHasContent(t *testing.T) {
	cases := map[string]bool{
		".":     false,
		"":      false,
		"，。":    false,
		"  ":    false,
		"嗯":     true,
		"开灯":    true,
		"你好小黑":  true,
		"? ...": false,
		"3 度":   true,
		"OK":    true,
	}
	for in, want := range cases {
		if got := hasContent(in); got != want {
			t.Errorf("hasContent(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestIsExitCommand(t *testing.T) {
	yes := []string{"停止", "退出", "再见", "退下", "停止吧", "结束", "别说了",
		"对下", // 实测「退下」被识别成「对下」（t/d 之差），必须容错
		"对下了"}
	for _, s := range yes {
		if !isExitCommand(s, nil) {
			t.Errorf("%q 应判为退出词", s)
		}
	}
	// 正常句子里的"停止/再见"不能退出
	no := []string{"把音乐停止播放", "跟他说明天见", "再见这个词怎么翻译", "", "帮我关灯"}
	for _, s := range no {
		if isExitCommand(s, nil) {
			t.Errorf("%q 不该判为退出词", s)
		}
	}
}
