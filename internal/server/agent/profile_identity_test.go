package agent

import (
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func TestEffectiveSystemPrompt_InjectsDisplayName(t *testing.T) {
	p := &Profile{
		ID: "default", Name: "管家",
		SystemPrompt: "你是家庭语音助手。回答要口语化、简短。",
	}
	got := p.EffectiveSystemPrompt()
	if !strings.Contains(got, "你的名字是「管家」") || !strings.Contains(got, "我是管家") {
		t.Fatalf("没把显示名称写进提示词: %q", got)
	}
	if !strings.Contains(got, "回答要口语化、简短") {
		t.Fatalf("不该覆盖用户自己的提示词: %q", got)
	}
	if !strings.HasPrefix(got, "你的名字是") {
		t.Fatalf("身份说明应放在最前面，模型才不会忽略: %q", got)
	}
}

// 老配置没有 name_in_prompt 字段（nil）时也要默认生效。
func TestEffectiveSystemPrompt_UnsetMeansEnabled(t *testing.T) {
	p := &Profile{Name: "小智"}
	if got := p.EffectiveSystemPrompt(); !strings.Contains(got, "我是小智") {
		t.Fatalf("老配置应默认注入名称: %q", got)
	}
}

// 自己写了另一套人格的用户可以显式关掉。
func TestEffectiveSystemPrompt_CanBeDisabled(t *testing.T) {
	p := &Profile{
		Name: "管家", NameInPrompt: boolPtr(false),
		SystemPrompt: "你是苏轼，号东坡居士。",
	}
	if got := p.EffectiveSystemPrompt(); got != "你是苏轼，号东坡居士。" {
		t.Fatalf("关掉后应原样返回自定义提示词: %q", got)
	}
}

func TestEffectiveSystemPrompt_EmptyName(t *testing.T) {
	p := &Profile{SystemPrompt: "保持简短"}
	if got := p.EffectiveSystemPrompt(); got != "保持简短" {
		t.Fatalf("没名字时不应注入: %q", got)
	}
	if got := (&Profile{}).EffectiveSystemPrompt(); got != "" {
		t.Fatalf("空档案应返回空: %q", got)
	}
	if got := (*Profile)(nil).EffectiveSystemPrompt(); got != "" {
		t.Fatalf("nil 档案应返回空: %q", got)
	}
}
