package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 天气来源选「联网搜索」时，应该用搜索配置去查「<城市> 今天天气」，
// 而不是回落到 HA 那个只有"晴/多云"的实体。
func TestWeatherFromSearch(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotQuery = string(buf[:n])
		_, _ = w.Write([]byte(`{"results":[{"title":"杭州天气预报","url":"https://weather.com.cn","content":"杭州今天多云转晴，18~26℃"}]}`))
	}))
	defer srv.Close()

	r := NewRegistry(nil)
	r.SetWeb(NewWebSearch(SearchOptions{Provider: "tavily", APIKey: "k", BaseURL: srv.URL}))
	r.SetWeather(NewWeather(WeatherOptions{Provider: "search", Location: "杭州"}), true)

	out := r.weatherText(context.Background())
	if !strings.Contains(out, "18~26") {
		t.Fatalf("应该用搜索结果当天气，实际：%q", out)
	}
	if !strings.Contains(gotQuery, "杭州") {
		t.Errorf("查询词应带上城市，实际请求体：%s", gotQuery)
	}
}

// 搜索不可用（没配 key）时要能退回 HA，别让用户听到"我查不了天气"。
func TestWeatherFallsBackToHAWhenSearchMissing(t *testing.T) {
	ha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/states") {
			_, _ = w.Write([]byte(`[{"entity_id":"weather.home","state":"sunny",
				"attributes":{"temperature":22,"humidity":55,"wind_speed":2}}]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ha.Close()

	r := NewRegistry(NewHA(ha.URL, "t"))
	r.SetWeather(NewWeather(WeatherOptions{Provider: "search", Location: "杭州"}), true)

	out := r.weatherText(context.Background())
	if !strings.Contains(out, "晴") || !strings.Contains(out, "22") {
		t.Fatalf("搜索不可用时应退回 HA 天气，实际：%q", out)
	}
}
