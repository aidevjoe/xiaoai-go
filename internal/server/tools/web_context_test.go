package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type allPrefetch struct{}

func (allPrefetch) Want(string) bool { return true }

// 天气/设备/搜索三类信息应该都拼进同一段「已知信息」里，
// 并且顺序固定（提示词稳定才容易命中前缀缓存）。
func TestContextInjectsSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"小米发布会","url":"https://example.com/a","content":"小米今天发布了新手机，售价 3999 元起"}]}`))
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	reg.SetWeb(NewWebSearch(SearchOptions{
		Provider: "tavily", APIKey: "k", BaseURL: srv.URL, MaxResults: 2,
	}))

	out := reg.Context(context.Background(), allPrefetch{}, "小米最新发布的手机多少钱")
	if !strings.Contains(out, "当前时间") {
		t.Errorf("时间应该始终注入：%q", out)
	}
	if !strings.Contains(out, "联网检索结果") || !strings.Contains(out, "3999") {
		t.Errorf("搜索没被注入：%q", out)
	}
}

// 不该搜的话就不搜，否则每轮平白多一次联网。
func TestContextSkipsSearchWhenNotNeeded(t *testing.T) {
	reg := NewRegistry(nil)
	reg.SetWeb(NewWebSearch(SearchOptions{Provider: "tavily", APIKey: "k", BaseURL: "http://127.0.0.1:1"}))
	out := reg.Context(context.Background(), allPrefetch{}, "把客厅的灯打开")
	if strings.Contains(out, "联网检索结果") {
		t.Errorf("这句话不该触发搜索：%q", out)
	}
}

// 搜索必须限时：慢的提供方不能把回答一起拖住。
func TestContextSearchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"results":[{"title":"慢结果","url":"https://slow.com","content":"不该等到这条"}]}`))
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	reg.SetWeb(NewWebSearch(SearchOptions{
		Provider: "tavily", APIKey: "k", BaseURL: srv.URL,
		Timeout: 5 * time.Second, // 客户端超时设长，靠预取预算兜住
	}))

	// 预取预算是 1.5s，测试里调小，免得为了验证超时要真等 1.5 秒
	old := prefetchBudget
	prefetchBudget = 300 * time.Millisecond
	defer func() { prefetchBudget = old }()

	start := time.Now()
	out := reg.Context(context.Background(), allPrefetch{}, "最新的新闻")
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("预取应在 %s 内放弃，实际用了 %s", prefetchBudget, elapsed)
	}
	if strings.Contains(out, "慢结果") {
		t.Fatal("超时后不该把迟到的结果拼进去")
	}
}

// 注入提示词的是给人读的一句话，不是 JSON：JSON 既费 token，
// 又容易让模型照着念字段名。
func TestHumanizeHAWeather(t *testing.T) {
	raw := `{"condition":"clear-night","humidity":90,"ok":true,"temperature":25,"wind_speed":3.2}`
	out := humanizeHAWeather(raw)
	for _, want := range []string{"晴（夜间）", "25°C", "湿度 90%"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q：%q", want, out)
		}
	}
	if strings.Contains(out, "condition") || strings.Contains(out, "{") {
		t.Errorf("不该原样保留 JSON：%q", out)
	}
}
