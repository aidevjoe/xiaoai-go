package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xiaoai-go/internal/server/llm"
)

// 「不用改代码就让助手多会一件事」：一条配置 = 一个工具。
func TestRegisterCustom_GETWithPathAndQuery(t *testing.T) {
	var gotPath, gotQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQ = r.URL.Query().Get("k")
		_, _ = w.Write([]byte("墨盒剩余 30%"))
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	errs := reg.RegisterCustom([]CustomToolSpec{{
		Name:        "printer_status",
		Description: "查询打印机状态",
		URL:         srv.URL + "/status/{which}",
		Params:      map[string]string{"which": "哪台打印机", "k": "附加查询串"},
		Required:    []string{"which"},
	}})
	if len(errs) != 0 {
		t.Fatalf("注册失败: %v", errs)
	}

	var found *llm.Tool
	for _, s := range reg.Specs(nil) {
		if s.Function.Name == "printer_status" {
			cp := s
			found = &cp
		}
	}
	if found == nil {
		t.Fatal("自定义工具没出现在工具定义里")
	}
	// 参数 schema 要能被模型读懂
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(found.Function.Parameters, &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	if schema.Type != "object" || schema.Properties["which"]["description"] != "哪台打印机" {
		t.Fatalf("schema 不对: %+v", schema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "which" {
		t.Fatalf("required 不对: %+v", schema.Required)
	}

	out, err := reg.Exec(context.Background(), "printer_status", `{"which":"书房 打印机","k":"1"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out != "墨盒剩余 30%" {
		t.Fatalf("返回值 = %q", out)
	}
	// 路径占位要 URL 转义（空格不能把 URL 打断）
	if gotPath != "/status/书房 打印机" && gotPath != "/status/书房%20打印机" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if gotQ != "1" {
		t.Fatalf("查询串 = %q", gotQ)
	}
}

func TestRegisterCustom_POSTBodyTemplate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("方法 = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %s", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	if errs := reg.RegisterCustom([]CustomToolSpec{{
		Name: "notify", URL: srv.URL, Method: "POST",
		Body:    `{"msg":"{text}","level":"info"}`,
		Params:  map[string]string{"text": "要说的话"},
		Headers: map[string]string{"X-Token": "abc"},
	}}); len(errs) != 0 {
		t.Fatalf("注册失败: %v", errs)
	}
	if _, err := reg.Exec(context.Background(), "notify", `{"text":"吃饭了"}`); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotBody["msg"] != "吃饭了" || gotBody["level"] != "info" {
		t.Fatalf("请求体 = %+v", gotBody)
	}
}

func TestRegisterCustom_RejectsBadConfig(t *testing.T) {
	reg := NewRegistry(nil)
	cases := []struct {
		name string
		spec CustomToolSpec
		want string
	}{
		{"缺名字", CustomToolSpec{URL: "http://x/"}, "name"},
		{"缺地址", CustomToolSpec{Name: "a"}, "url"},
		{"协议不对", CustomToolSpec{Name: "b", URL: "file:///etc/passwd"}, "http"},
		{"与内置重名", CustomToolSpec{Name: "ha_get_state", URL: "http://x/"}, "重名"},
	}
	for _, c := range cases {
		errs := reg.RegisterCustom([]CustomToolSpec{c.spec})
		if len(errs) == 0 {
			t.Fatalf("%s：应该报错", c.name)
		}
		if !strings.Contains(errs[0].Error(), c.want) {
			t.Fatalf("%s：错误信息应包含 %q，实际 %v", c.name, c.want, errs[0])
		}
	}
}

// 接口报错要把状态码和对端原因带出来：只说"调用失败"没法排查。
func TestCustomTool_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "打印机离线", http.StatusInternalServerError)
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	if errs := reg.RegisterCustom([]CustomToolSpec{{Name: "p", URL: srv.URL}}); len(errs) != 0 {
		t.Fatalf("注册失败: %v", errs)
	}
	// Exec 故意把错误变成喂给模型的文本（不让一次工具失败毁掉整轮对话），
	// 所以这里断言的是返回内容，不是 error。
	out, err := reg.Exec(context.Background(), "p", "{}")
	if err != nil {
		t.Fatalf("Exec 不该返回 error: %v", err)
	}
	if !strings.Contains(out, "500") || !strings.Contains(out, "打印机离线") {
		t.Fatalf("错误信息不完整: %s", out)
	}
}

// 长响应会顶爆模型上下文，必须截断。
func TestCustomTool_ResponseIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("很长的一段内容", 500)))
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	if errs := reg.RegisterCustom([]CustomToolSpec{{Name: "big", URL: srv.URL, MaxBytes: 64}}); len(errs) != 0 {
		t.Fatalf("注册失败: %v", errs)
	}
	out, err := reg.Exec(context.Background(), "big", "{}")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len([]rune(out)) > 80 {
		t.Fatalf("响应没有截断，长度 %d", len([]rune(out)))
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("截断后应提示，实际 %q", out)
	}
}

// Unregister 之后不能再调用（后台删掉一条自定义工具后必须立刻失效）。
func TestCustomTool_Unregister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	if errs := reg.RegisterCustom([]CustomToolSpec{{Name: "tmp", URL: srv.URL}}); len(errs) != 0 {
		t.Fatalf("注册失败: %v", errs)
	}
	reg.Unregister("tmp")
	out, err := reg.Exec(context.Background(), "tmp", "{}")
	if err != nil {
		t.Fatalf("Exec 不该返回 error: %v", err)
	}
	if !strings.Contains(out, "未知工具") {
		t.Fatalf("摘掉之后应报未知工具，实际 %s", out)
	}
	for _, n := range reg.AllNames() {
		if n == "tmp" {
			t.Fatal("AllNames 里还留着已摘掉的工具")
		}
	}
}
