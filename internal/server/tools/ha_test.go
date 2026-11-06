package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeHA(t *testing.T, states string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/states":
			fmt.Fprint(w, states)
		case strings.HasPrefix(r.URL.Path, "/api/states/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/states/")
			fmt.Fprintf(w, `{"entity_id":%q,"state":"on","attributes":{"friendly_name":"测试灯"}}`, id)
		case strings.HasPrefix(r.URL.Path, "/api/services/"):
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const sampleStates = `[
 {"entity_id":"light.keting_1","state":"on","attributes":{"friendly_name":"客厅筒灯1"}},
 {"entity_id":"light.keting_2","state":"off","attributes":{"friendly_name":"客厅筒灯2"}},
 {"entity_id":"switch.zoulang_keting","state":"on","attributes":{"friendly_name":"走廊头开关 客厅灯 开关状态切换"}},
 {"entity_id":"sensor.x","state":"1","attributes":{"friendly_name":"设备 电量"}},
 {"entity_id":"weather.home","state":"rainy","attributes":{"temperature":30.1,"humidity":58}}
]`

func TestSearchRanksRealLightFirst(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))

	out, err := r.Exec(context.Background(), "ha_search_entities", `{"query":"客厅的灯"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Count    int `json:"count"`
		Entities []struct {
			EntityID string `json:"entity_id"`
		} `json:"entities"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Count == 0 {
		t.Fatalf("应有结果: %s", out)
	}
	// 指示灯类实体应被过滤；真正的灯要排在前面
	for _, e := range res.Entities {
		if strings.HasPrefix(e.EntityID, "sensor.") {
			t.Fatalf("不该返回诊断类实体: %s", e.EntityID)
		}
	}
	first := res.Entities[0].EntityID
	if first != "light.keting_1" && first != "light.keting_2" && first != "switch.zoulang_keting" {
		t.Fatalf("首条应是真正的客厅灯，实际 %s", first)
	}
}

func TestCallServiceBlockedDomains(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))
	for _, d := range []string{"lock", "alarm_control_panel", "script", "shell_command"} {
		out, _ := r.Exec(context.Background(), "ha_call_service",
			fmt.Sprintf(`{"domain":%q,"service":"turn_on","entity_id":"x.y"}`, d))
		if !strings.Contains(out, "不允许") {
			t.Fatalf("%s 域应被拒绝，实际: %s", d, out)
		}
	}
}

func TestCallServiceReturnsState(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))
	out, err := r.Exec(context.Background(), "ha_call_service",
		`{"domain":"light","service":"turn_on","entity_id":"light.keting_1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"ok":true`) || !strings.Contains(out, `"state":"on"`) {
		t.Fatalf("应回读状态，实际: %s", out)
	}
}

func TestWeatherTool(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))
	out, _ := r.Exec(context.Background(), "ha_get_weather", `{}`)
	if !strings.Contains(out, "雨") {
		t.Fatalf("天气应转成中文，实际: %s", out)
	}
	if !strings.Contains(out, "30.1") {
		t.Fatalf("应带温度，实际: %s", out)
	}
}

func TestTimeTool(t *testing.T) {
	r := NewRegistry(nil)
	out, _ := r.Exec(context.Background(), "get_current_time", `{}`)
	if !strings.Contains(out, "datetime") || !strings.Contains(out, "星期") {
		t.Fatalf("时间工具输出不对: %s", out)
	}
}

func TestUnknownTool(t *testing.T) {
	r := NewRegistry(nil)
	out, _ := r.Exec(context.Background(), "nope", `{}`)
	if !strings.Contains(out, "未知工具") {
		t.Fatalf("应报未知工具: %s", out)
	}
}

func TestBadArgs(t *testing.T) {
	r := NewRegistry(nil)
	out, _ := r.Exec(context.Background(), "ha_search_entities", `{bad json`)
	if !strings.Contains(out, "不合法") && !strings.Contains(out, "合法 JSON") {
		t.Fatalf("应报参数错误: %s", out)
	}
}

func TestSpecsFilter(t *testing.T) {
	r := NewRegistry(nil)
	all := r.Specs(nil)
	// 只暴露 3 个动作类工具；时间/天气走 Context 预取，
	// 实测每多一个工具首字就明显变慢（5 个时 7.8s，2 个时 1.0s）。
	if len(all) != 3 {
		t.Fatalf("应有 3 个工具，实际 %d", len(all))
	}
	names := map[string]bool{}
	for _, t := range all {
		names[t.Function.Name] = true
	}
	for _, forbidden := range []string{"get_current_time", "ha_get_weather"} {
		if names[forbidden] {
			t.Fatalf("%s 应改为上下文预取，不该占用工具名额", forbidden)
		}
	}
	for _, want := range []string{"ha_search_entities", "ha_get_state", "ha_call_service"} {
		if !names[want] {
			t.Fatalf("缺少工具 %s", want)
		}
	}

	only := r.Specs(func(name string) bool { return name == "ha_call_service" })
	if len(only) != 1 || only[0].Function.Name != "ha_call_service" {
		t.Fatalf("过滤失败: %+v", only)
	}
}

// allOn 是所有预取都开启的策略（等价于档案未配置 prefetch）。
type allOn struct{}

func (allOn) Want(string) bool { return true }

// onlyTime 只开时间预取。
type onlyTime struct{}

func (onlyTime) Want(k string) bool { return k == "time" }

func TestContextPrefetchesTime(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))
	out := r.Context(context.Background(), allOn{}, "现在几点了")
	if !strings.Contains(out, "当前时间") {
		t.Fatalf("应预取当前时间: %s", out)
	}
	if strings.Contains(out, "天气") {
		t.Fatal("问时间不该顺便拉天气")
	}
}

func TestContextPrefetchesWeatherOnlyWhenAsked(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))
	out := r.Context(context.Background(), allOn{}, "今天天气怎么样")
	if !strings.Contains(out, "天气实况") {
		t.Fatalf("问天气应预取: %s", out)
	}
	if !strings.Contains(out, "雨") {
		t.Fatalf("天气内容缺失: %s", out)
	}
}

func TestContextPrefetchesDevicesOnlyWhenAsked(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))
	out := r.Context(context.Background(), allOn{}, "客厅的灯开着吗")
	if !strings.Contains(out, "可能相关的设备") {
		t.Fatalf("问设备应预搜: %s", out)
	}
	if !strings.Contains(out, "客厅筒灯") {
		t.Fatalf("设备候选缺失: %s", out)
	}

	// 闲聊时不该塞设备列表，白占 token
	plain := r.Context(context.Background(), allOn{}, "给我讲个笑话")
	if strings.Contains(plain, "可能相关的设备") {
		t.Fatalf("闲聊不该预搜设备: %s", plain)
	}
}

func TestContextWithoutHA(t *testing.T) {
	r := NewRegistry(nil)
	out := r.Context(context.Background(), allOn{}, "今天天气怎么样")
	if !strings.Contains(out, "当前时间") {
		t.Fatalf("没有 HA 也应能给出时间: %s", out)
	}
}

// 关掉预取的档案（不接 HA 的用户）不应该收到任何注入。
func TestContextRespectsPrefetchSwitches(t *testing.T) {
	srv := fakeHA(t, sampleStates)
	r := NewRegistry(NewHA(srv.URL, "t"))

	// 只开时间：问天气时不该出现天气，问设备时不该出现设备
	weather := r.Context(context.Background(), onlyTime{}, "今天天气怎么样")
	if strings.Contains(weather, "天气实况") {
		t.Fatalf("天气预取已关闭却仍注入: %s", weather)
	}
	devices := r.Context(context.Background(), onlyTime{}, "客厅的灯开着吗")
	if strings.Contains(devices, "可能相关的设备") {
		t.Fatalf("设备预取已关闭却仍注入: %s", devices)
	}
	if !strings.Contains(devices, "当前时间") {
		t.Fatalf("时间预取仍应保留: %s", devices)
	}
}

// 后台关掉的工具既不该出现在工具定义里，也不该被执行。
func TestGlobalDisableHidesAndRejectsTool(t *testing.T) {
	srv := fakeHA(t, `[{"entity_id":"light.a","state":"on","attributes":{}}]`)
	defer srv.Close()
	r := NewRegistry(NewHA(srv.URL, "t"))
	r.SetEnabled([]string{"ha_get_state"})

	for _, spec := range r.Specs(nil) {
		if spec.Function.Name == "ha_call_service" {
			t.Fatal("被关掉的工具不该出现在定义里")
		}
	}
	out, err := r.Exec(context.Background(), "ha_call_service", `{"domain":"light","service":"turn_on","entity_id":"light.a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "关闭") {
		t.Fatalf("被关掉的工具应拒绝执行，实际: %s", out)
	}

	r.SetEnabled(nil) // 空 = 全部启用
	if len(r.Specs(nil)) == 0 {
		t.Fatal("清空开关后应恢复全部工具")
	}
}
