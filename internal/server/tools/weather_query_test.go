package tools

import "testing"

// 查询词必须能被中文搜索正确理解：实测「杭州 今天天气」会搜出惠州，
// 「杭州天气」才对。
func TestWeatherSearchQuery(t *testing.T) {
	w := NewWeather(WeatherOptions{Provider: "search", Location: "杭州"})
	if got := w.SearchQuery(); got != "杭州天气" {
		t.Fatalf("查询词应为「杭州天气」，实际 %q", got)
	}
	if w := NewWeather(WeatherOptions{Provider: "search"}); w.SearchQuery() == "" {
		t.Fatal("没填城市时也该有个能用的查询词")
	}
}

// 搜索结果跑偏（城市不对）必须判废，宁可退回 HA 也别答错城市。
func TestWeatherSearchRelevant(t *testing.T) {
	w := NewWeather(WeatherOptions{Provider: "search", Location: "杭州"})
	if !w.Relevant("杭州天气预报，今天多云 18~26℃") {
		t.Fatal("结果里有城市名，应该判为可用")
	}
	if w.Relevant("惠州天气预报，局部雷雨 23~32℃") {
		t.Fatal("结果里是别的城市，必须判废")
	}
	// 英文地名不做这个校验（中文检索本来就常常返回英文页面）
	en := NewWeather(WeatherOptions{Provider: "search", Location: "Hangzhou"})
	if !en.Relevant("Hangzhou monthly weather") {
		t.Fatal("英文地名不该被这条规则拦掉")
	}
}
