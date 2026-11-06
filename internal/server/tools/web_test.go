package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 语音助手最怕"什么都搜"：每轮多一次联网就是每轮慢 1 秒，
// 所以触发词必须保守，问"你是谁"这类不能触发。
func TestShouldSearch(t *testing.T) {
	w := NewWebSearch(SearchOptions{Provider: "bocha", APIKey: "k"})
	if w == nil {
		t.Fatal("应该构造成功")
	}
	yes := []string{"今天有什么新闻", "小米最新发布的手机多少钱", "帮我查一下汇率"}
	for _, s := range yes {
		if _, ok := w.ShouldSearch(s); !ok {
			t.Errorf("%q 应该触发搜索", s)
		}
	}
	no := []string{"你是谁", "把客厅的灯打开", "退下", "现在几点"}
	for _, s := range no {
		if _, ok := w.ShouldSearch(s); ok {
			t.Errorf("%q 不该触发搜索", s)
		}
	}
}

// 每个服务商返回的 JSON 结构都不一样，解析错一个字段就等于"搜了但没结果"。
func TestSearchProviders(t *testing.T) {
	cases := []struct {
		provider string
		body     string
		want     string
	}{
		{
			provider: "bocha",
			body: `{"data":{"webPages":{"value":[
				{"name":"小米发布会","url":"https://example.com/a","summary":"小米今天发布了新手机"},
				{"name":"第二条","url":"https://b.com/x","summary":"另一个结果"}]}}}`,
			want: "小米发布会",
		},
		{
			provider: "tavily",
			body:     `{"results":[{"title":"小米发布会","url":"https://example.com/a","content":"小米今天发布了新手机"}]}`,
			want:     "小米发布会",
		},
		{
			provider: "serper",
			body:     `{"organic":[{"title":"小米发布会","link":"https://example.com/a","snippet":"小米今天发布了新手机"}]}`,
			want:     "小米发布会",
		},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(c.body))
		}))
		ws := NewWebSearch(SearchOptions{
			Provider: c.provider, APIKey: "k", BaseURL: srv.URL, MaxResults: 3,
		})
		out, err := ws.Fetch(context.Background(), "小米发布")
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.provider, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s 解析失败，结果=%q", c.provider, out)
		}
		// 域名要带上，模型才知道信息来自哪里
		if !strings.Contains(out, "example.com") && c.provider != "tavily" {
			t.Errorf("%s 应带来源域名，结果=%q", c.provider, out)
		}
	}
}

// 结果不能太长，否则一段网页正文就把提示词塞满。
func TestSearchTruncatesSnippet(t *testing.T) {
	long := strings.Repeat("好", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"标题","url":"https://a.com","content":"` + long + `"}]}`))
	}))
	defer srv.Close()
	ws := NewWebSearch(SearchOptions{Provider: "tavily", APIKey: "k", BaseURL: srv.URL})
	out, err := ws.Fetch(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(out)); n > 200 {
		t.Fatalf("摘要应该被截断，实际 %d 字：%q", n, out)
	}
}

// 和风天气的字段是字符串（"12" 不是 12），解析漏了就会得到空温度。
func TestWeatherQWeather(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		if strings.Contains(r.URL.Path, "/3d") {
			_, _ = w.Write([]byte(`{"code":"200","daily":[
				{"fxDate":"2026-11-06","tempMax":"18","tempMin":"9","textDay":"晴","textNight":"多云"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"200","now":{"temp":"15","feelsLike":"13","text":"晴",
			"windDir":"西北风","windScale":"3","humidity":"40","obsTime":"2026-11-05T20:00+08:00"}}`))
	}))
	defer srv.Close()

	w := NewWeather(WeatherOptions{
		Provider: "qweather", APIKey: "k", Location: "101010100", Days: 3,
		BaseURL: srv.URL, // 测试里指向本地假服务
	})
	out, err := w.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"晴", "15°C", "湿度 40%", "11-06"} {
		if !strings.Contains(out, want) {
			t.Errorf("天气描述缺少 %q：%q", want, out)
		}
	}
	if !strings.Contains(lastPath, "3d") {
		t.Errorf("填了预报天数就该请求 /3d，实际走的是 %q", lastPath)
	}
}

// 没配 key 就不该假装能用（否则每轮白白多一次失败请求）。
func TestWeatherNotReady(t *testing.T) {
	w := NewWeather(WeatherOptions{Provider: "qweather"})
	if w.Ready() {
		t.Fatal("没有 key 时 Ready 必须为 false")
	}
	if _, err := w.Current(context.Background()); err == nil {
		t.Fatal("没配置时应返回错误")
	}
}
