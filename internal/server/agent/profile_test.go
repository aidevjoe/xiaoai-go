package agent

import "testing"

func sampleProfiles() []*Profile {
	return []*Profile{
		{
			ID: "steward", Name: "管家", WakeWords: []string{"小黑", "你好小黑"},
			SystemPrompt: "你是家庭管家",
			LLM:          LLM{BaseURL: "https://a/v1", Model: "deepseek-flash"},
		},
		{
			ID: "chat", Name: "闲聊", WakeWords: []string{"小智"},
			SystemPrompt: "你是聊天伙伴",
			LLM:          LLM{BaseURL: "https://b/v1", Model: "kimi"},
			Tools:        []string{"none"},
		},
	}
}

func TestMatchExact(t *testing.T) {
	r, err := NewRegistry(sampleProfiles())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Match("小黑").ID; got != "steward" {
		t.Fatalf("应命中 steward，实际 %s", got)
	}
	if got := r.Match("你好小黑").ID; got != "steward" {
		t.Fatalf("应命中 steward，实际 %s", got)
	}
	if got := r.Match("小智").ID; got != "chat" {
		t.Fatalf("应命中 chat，实际 %s", got)
	}
}

func TestMatchWithNoise(t *testing.T) {
	r, _ := NewRegistry(sampleProfiles())
	cases := []string{
		"你好 小黑。", // 空格与标点
		"小黑小黑",   // KWS 重复
		"嗯小黑",    // 前置语气词
		"你好小",    // 识别丢字（包含关系）
	}
	for _, c := range cases {
		if got := r.MatchID(c); got != "steward" {
			t.Errorf("%q 应命中 steward，实际 %q", c, got)
		}
	}
}

func TestMatchFallback(t *testing.T) {
	r, _ := NewRegistry(sampleProfiles())
	// 完全无关的词回落到第一个档案，避免"叫不应"
	if got := r.Match("今天天气").ID; got != "steward" {
		t.Fatalf("应回落 steward，实际 %s", got)
	}
	if id := r.MatchID(""); id != "steward" {
		t.Fatalf("空词应回落，实际 %s", id)
	}
}

func TestProfileRoutingToDifferentLLM(t *testing.T) {
	r, _ := NewRegistry(sampleProfiles())
	a := r.Match("小黑")
	b := r.Match("小智")
	if a.LLM.BaseURL == b.LLM.BaseURL {
		t.Fatal("两个档案应指向不同的模型端点")
	}
	if a.LLM.Model == b.LLM.Model {
		t.Fatal("两个档案应使用不同模型")
	}
	if a.AllowsTool("ha_call_service") != true {
		t.Fatal("管家应能用 HA 工具")
	}
	if b.AllowsTool("ha_call_service") != false {
		t.Fatal("闲聊档案应禁用工具")
	}
}

func TestDuplicateWakeWordRejected(t *testing.T) {
	ps := sampleProfiles()
	ps[1].WakeWords = append(ps[1].WakeWords, "小黑")
	if _, err := NewRegistry(ps); err == nil {
		t.Fatal("重复唤醒词应报错")
	}
}

func TestInvalidProfileRejected(t *testing.T) {
	cases := []*Profile{
		{ID: "", WakeWords: []string{"a"}, LLM: LLM{BaseURL: "x", Model: "m"}},
		{ID: "a", WakeWords: nil, LLM: LLM{BaseURL: "x", Model: "m"}},
		{ID: "a", WakeWords: []string{"a"}, LLM: LLM{Model: "m"}},
	}
	for i, p := range cases {
		if _, err := NewRegistry([]*Profile{p}); err == nil {
			t.Errorf("第 %d 个非法档案应被拒绝", i)
		}
	}
	if _, err := NewRegistry(nil); err == nil {
		t.Error("空档案表应被拒绝")
	}
}

func TestHotReload(t *testing.T) {
	r, _ := NewRegistry(sampleProfiles())
	if r.Match("翻译").ID != "steward" {
		t.Fatal("初始不该有翻译档案")
	}
	ps := sampleProfiles()
	ps = append(ps, &Profile{
		ID: "translate", Name: "翻译", WakeWords: []string{"翻译"},
		LLM: LLM{BaseURL: "https://c/v1", Model: "glm"},
	})
	if err := r.Set(ps); err != nil {
		t.Fatal(err)
	}
	if r.Match("翻译").LLM.Model != "glm" {
		t.Fatal("热更新后应能匹配新档案")
	}
	if len(r.All()) != 3 {
		t.Fatalf("应有 3 个档案，实际 %d", len(r.All()))
	}
}

func TestExitWords(t *testing.T) {
	r, _ := NewRegistry(sampleProfiles())
	p := r.Match("小黑")
	if !p.IsExit("退下吧") {
		t.Fatal("应识别退出词")
	}
	if p.IsExit("打开灯") {
		t.Fatal("普通指令不该判为退出")
	}
}

func TestSaveLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/profiles.json"
	if err := SaveFile(path, sampleProfiles()); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "steward" {
		t.Fatalf("读回不一致: %+v", got)
	}
	if got[1].LLM.Model != "kimi" {
		t.Fatalf("模型字段丢失: %+v", got[1].LLM)
	}
}
