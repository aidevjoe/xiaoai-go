package tools

// 配置化的自定义工具。
//
// 内置的 HA 三件套是写死的，而「让助手多做一件事」往往是这种需求：
// 「帮我查一下我自建服务里的某个数」，或者「调一下家里的打印服务」。
// 为此专门写一个 Go 文件、重新编译、部署一台 NAS 上的服务，门槛太高。
//
// 所以这里提供一个纯配置的 HTTP 工具：
//
//	tools.custom = [{
//	  "name": "printer_status",
//	  "description": "查询打印机状态与墨盒余量",
//	  "url": "http://192.168.0.30:8080/status",
//	  "method": "GET",
//	  "params": {"device": "打印机名，留空查全部"},
//	  "profiles": []            // 空=所有助手都能用
//	}]
//
// 参数用 {名字} 占位符同时出现在 url / body / headers 里，
// 这样 GET 查询串和 POST 请求体都不用另学一套模板语法。
//
// 安全边界（重要）：
//   - 只允许 http/https，不允许 file:// 之类；
//   - 响应截断到 4KB，避免把模型上下文顶爆；
//   - 仍然受档案 Tools 白名单与全局 blocked 域名约束；
//   - 这是给"用户自己配自己的服务"用的，不要填公网不可信地址。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"xiaoai-go/internal/server/llm"
)

// CustomToolSpec 是一条自定义工具的配置（与 store 里的结构一一对应）。
// 放在 tools 包里而不是 store 里：注册逻辑需要它，store 只需要 JSON 字段。
type CustomToolSpec struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	URL         string            `json:"url"`
	Method      string            `json:"method"` // GET / POST，默认 GET
	Params      map[string]string `json:"params"` // 参数名 → 给模型看的说明
	Required    []string          `json:"required"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`       // POST 请求体模板（JSON），支持 {参数}
	TimeoutMS   int               `json:"timeout_ms"` // 默认 5000
	MaxBytes    int               `json:"max_bytes"`  // 响应截断，默认 4096
}

// AllNames 返回全部工具名（内置 + 自定义），后台勾选列表用。
func (r *Registry) AllNames() []string {
	out := Names()
	if r == nil {
		return out
	}
	r.mu.RLock()
	for n := range r.custom {
		out = append(out, n)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// customHTTPTool 把一条配置变成可注册的工具。
func (r *Registry) RegisterCustom(specs []CustomToolSpec) []error {
	var errs []error
	for _, sp := range specs {
		if err := r.registerCustomOne(sp); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (r *Registry) registerCustomOne(sp CustomToolSpec) error {
	name := strings.TrimSpace(sp.Name)
	if name == "" {
		return fmt.Errorf("自定义工具缺少 name")
	}
	if strings.TrimSpace(sp.URL) == "" {
		return fmt.Errorf("自定义工具 %s 缺少 url", name)
	}
	u, err := url.Parse(sp.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("自定义工具 %s 的 url 只支持 http/https: %s", name, sp.URL)
	}
	if _, exists := r.specByName(name); exists {
		return fmt.Errorf("工具 %s 已存在，自定义工具不能和内置工具重名", name)
	}

	// 参数 schema：全是 string。让模型填结构化的东西反而更容易出错，
	// 而我们的使用方式就是"塞进 URL / 请求体"。
	props := make(map[string]any, len(sp.Params))
	names := make([]string, 0, len(sp.Params))
	for k := range sp.Params {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		props[k] = map[string]any{"type": "string", "description": sp.Params[k]}
	}
	schema, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": props,
		"required":   sp.Required,
	})
	if err != nil {
		return fmt.Errorf("自定义工具 %s 的参数生成失败: %w", name, err)
	}

	desc := strings.TrimSpace(sp.Description)
	if desc == "" {
		desc = "调用 " + u.Host + " 上的自定义接口"
	}
	method := strings.ToUpper(strings.TrimSpace(sp.Method))
	if method == "" {
		method = http.MethodGet
	}
	timeout := time.Duration(sp.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	maxBytes := sp.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	spec := sp // 拷一份，避免调用方后续改动影响已注册的工具

	r.Register(llm.Tool{
		Type: "function",
		Function: llm.ToolSpec{
			Name:        name,
			Description: desc,
			Parameters:  schema,
		},
	}, func(ctx context.Context, args map[string]any) (string, error) {
		return callCustomHTTP(ctx, spec, method, timeout, maxBytes, args)
	})
	return nil
}

// callCustomHTTP 执行一次自定义 HTTP 工具。
func callCustomHTTP(ctx context.Context, sp CustomToolSpec,
	method string, timeout time.Duration, maxBytes int, args map[string]any) (string, error) {

	fill := func(s string) string { return substParams(s, args) }

	target := fill(sp.URL)
	if method == http.MethodGet || method == http.MethodHead {
		// GET 的参数拼到查询串上：{k} 已在 URL 里替换过，
		// 这里把"模型给了但 URL 里没占位"的参数补进去，省得用户必须写全模板。
		if u, err := url.Parse(target); err == nil {
			q := u.Query()
			for k, v := range args {
				if v == nil || strings.Contains(sp.URL, "{"+k+"}") {
					continue
				}
				q.Set(k, toStr(v))
			}
			u.RawQuery = q.Encode()
			target = u.String()
		}
	}

	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		raw := strings.TrimSpace(sp.Body)
		if raw == "" {
			// 没写模板就整个参数表当 JSON 发过去
			b, err := json.Marshal(args)
			if err != nil {
				return "", fmt.Errorf("构造请求体失败: %w", err)
			}
			body = bytes.NewReader(b)
		} else {
			body = strings.NewReader(fill(raw))
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, target, body)
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range sp.Headers {
		req.Header.Set(k, fill(v))
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 %s 失败: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	// 多读一些再按字符截断：直接按字节截会把一个汉字劈成两半，
	// 模型收到的就是乱码（实测出现 "一段内容\xe5" 这种尾巴）。
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)*4+1))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("接口返回 %d: %s", resp.StatusCode, clipText(string(data), 200))
	}
	out := strings.TrimSpace(string(data))
	if out == "" {
		return "（接口没有返回内容）", nil
	}
	if maxBytes > 0 && len([]rune(out)) > maxBytes {
		return clipText(out, maxBytes) + "…（已截断）", nil
	}
	return out, nil
}

// substParams 把 {名字} 换成参数值；没给的参数保留原样。
//
// 这里刻意**不**做 URL 转义：同一个函数既要处理 URL 的路径段，
// 也要处理 JSON 请求体和请求头——对后者做 QueryEscape 会把中文变成
// %E5%90%83%E9%A5%AD 这种，服务端收到的就是字面量。
// URL 的转义交给 net/url：它会在真正发请求时把空格等字符编码掉。
func substParams(tpl string, args map[string]any) string {
	if !strings.Contains(tpl, "{") {
		return tpl
	}
	out := tpl
	for k, v := range args {
		if v == nil {
			continue
		}
		out = strings.ReplaceAll(out, "{"+k+"}", toStr(v))
	}
	return out
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// 模型给的数字都是 float64；整数就别显示成 3.000000
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

// specByName 判断是否已存在同名工具（内置或之前注册的自定义工具）。
func (r *Registry) specByName(name string) (llm.Tool, bool) {
	for _, t := range r.Specs(nil) {
		if t.Function.Name == name {
			return t, true
		}
	}
	return llm.Tool{}, false
}

// sharedClient 给自定义工具共用：每次调用都新建 Transport 会漏连接。
var sharedClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	},
}
