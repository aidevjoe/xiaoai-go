package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Go 里 string[i] 取的是字节，中文按字节切会得到半个字符。
// 线上真出过这事：提示词里写成"星期º"，模型只能瞎猜。
func TestWeekdayCN(t *testing.T) {
	want := map[time.Weekday]string{
		time.Sunday: "日", time.Monday: "一", time.Tuesday: "二", time.Wednesday: "三",
		time.Thursday: "四", time.Friday: "五", time.Saturday: "六",
	}
	for d, w := range want {
		if got := weekdayCN(d); got != w {
			t.Errorf("%v 应得 %q，实际 %q", d, w, got)
		}
	}
}

// 注入的时间必须能被正常读出（不能出现乱码）。
func TestContextTimeIsReadable(t *testing.T) {
	r := NewRegistry(nil)
	out := r.Context(context.Background(), allPrefetch{}, "现在几点了")
	if !strings.Contains(out, "当前时间") || !strings.Contains(out, "星期") {
		t.Fatalf("时间注入不完整：%q", out)
	}
	if strings.ContainsAny(out, "ºª") || strings.ContainsRune(out, '\uFFFD') {
		t.Fatalf("时间注入出现乱码：%q", out)
	}
}
