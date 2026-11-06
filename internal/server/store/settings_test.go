package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"xiaoai-go/internal/server/agent"
)

func sample() Settings {
	s := Defaults()
	s.Profiles = []*agent.Profile{{
		ID: "default", Name: "管家", WakeWords: []string{"小黑"},
		SystemPrompt: "你是助手",
		LLM:          agent.LLM{BaseURL: "https://x/v1", Model: "m"},
	}}
	return s
}

func TestOpenCreatesFileWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	m, err := Open(path, sample())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应自动创建配置文件: %v", err)
	}
	got := m.Get()
	if len(got.Profiles) != 1 || got.Dialog.TimeoutSec == 0 {
		t.Fatalf("默认值不完整: %+v", got)
	}
	// Engine.Threads 的 0 现在是"按设备核数自动"的哨兵值，不是"没填"，
	// 所以这里不能再断言它必须 > 0；改成检查别的分段确实补上了默认值。
	if got.Server.Name == "" || got.Voice.Speed == 0 || got.Engine.VADThreshold == 0 {
		t.Fatalf("分段默认值未补齐: %+v", got)
	}
	if got.Engine.Threads < 0 {
		t.Fatalf("线程数不该是负数（0 = 自动）: %d", got.Engine.Threads)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	m, err := Open(path, sample())
	if err != nil {
		t.Fatal(err)
	}
	s := sample()
	s.Profiles[0].Name = "改过的名字"
	s.Dialog.TimeoutSec = 45
	s.Engine.Threads = 3
	s.Voice.Speed = 1.2
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}

	m2, err := Open(path, sample())
	if err != nil {
		t.Fatal(err)
	}
	got := m2.Get()
	if got.Profiles[0].Name != "改过的名字" || got.Dialog.TimeoutSec != 45 {
		t.Fatalf("重载后不一致: %+v", got)
	}
	if got.Engine.Threads != 3 || got.Voice.Speed != 1.2 {
		t.Fatalf("分段配置未持久化: %+v", got)
	}
}

// 非法配置必须在落盘前被拒绝，否则会把正在跑的系统配坏。
func TestSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	m, err := Open(path, sample())
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(*Settings){
		"没有档案":  func(s *Settings) { s.Profiles = nil },
		"缺模型地址": func(s *Settings) { s.Profiles[0].LLM.BaseURL = "" },
		"没有唤醒词": func(s *Settings) { s.Profiles[0].WakeWords = nil },
		"档案为空":  func(s *Settings) { s.Profiles = append(s.Profiles, nil) },
		"档案 id 重复": func(s *Settings) {
			s.Profiles = append(s.Profiles, &agent.Profile{
				ID: "default", WakeWords: []string{"小智"},
				LLM: agent.LLM{BaseURL: "u", Model: "m"},
			})
		},
		"唤醒词重复": func(s *Settings) {
			s.Profiles = append(s.Profiles, &agent.Profile{
				ID: "b", WakeWords: []string{"小黑"},
				LLM: agent.LLM{BaseURL: "u", Model: "m"},
			})
		},
	}
	for name, mutate := range cases {
		s := sample()
		mutate(&s)
		if err := m.Save(s); err == nil {
			t.Fatalf("%s：应被拒绝", name)
		}
	}
	// 拒绝之后内存里的配置不该被污染
	if m.Get().Profiles[0].WakeWords[0] != "小黑" {
		t.Fatal("非法保存污染了内存配置")
	}
}

// 原子写：不能留下 .tmp 垃圾文件。
func TestSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	m, _ := Open(path, sample())
	if err := m.Save(sample()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}

// 文件被写坏时要明确报错，而不是静默用空配置启动。
func TestOpenRejectsBrokenJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o644)

	if _, err := Open(path, sample()); err == nil {
		t.Fatal("损坏的配置应报错")
	}
}

// Normalize 应补齐空字段，保证老配置升级后仍可用。
func TestNormalizeFillsEverySection(t *testing.T) {
	s := sample()
	s.Server = Server{}
	s.Engine = Engine{}
	s.Voice = Voice{}
	s.Dialog = Dialog{}
	s.Tools = Tools{}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if s.Server.Name == "" || s.Server.KeepTurns == 0 {
		t.Fatalf("server 未补齐: %+v", s.Server)
	}
	if s.Engine.ModelDir == "" || s.Engine.VADThreshold == 0 || s.Engine.KWSThreshold == 0 {
		t.Fatalf("engine 未补齐: %+v", s.Engine)
	}
	if s.Voice.Speed == 0 || s.Voice.MaxSentenceRunes == 0 {
		t.Fatalf("voice 未补齐: %+v", s.Voice)
	}
	if s.Dialog.SilenceMS == 0 || s.Dialog.TimeoutSec == 0 || s.Dialog.MaxRounds == 0 {
		t.Fatalf("dialog 未补齐: %+v", s.Dialog)
	}
	if len(s.Tools.Blocked) == 0 {
		t.Fatalf("tools 未补齐: %+v", s.Tools)
	}

	b, _ := json.Marshal(s)
	if len(b) == 0 {
		t.Fatal("序列化失败")
	}
}

// 阈值给到离谱值时要被夹回合理区间，否则表现为"永远不响应"。
func TestNormalizeClampsRanges(t *testing.T) {
	s := sample()
	s.Engine.Threads = 999
	s.Engine.VADThreshold = 7
	s.Voice.Speed = 0.0001
	s.Server.KeepTurns = 99999
	s.Dialog.PreRollMS = 5000
	s.Dialog.MinSpeechMS = 100
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if s.Engine.Threads > 16 {
		t.Fatalf("线程数未夹紧: %d", s.Engine.Threads)
	}
	if s.Engine.VADThreshold > 1 {
		t.Fatalf("VAD 阈值未夹紧: %v", s.Engine.VADThreshold)
	}
	if s.Voice.Speed <= 0 || s.Voice.Speed > 3 {
		t.Fatalf("语速未夹紧: %v", s.Voice.Speed)
	}
	if s.Server.KeepTurns > 5000 {
		t.Fatalf("保留轮数未夹紧: %d", s.Server.KeepTurns)
	}
	// 预滚的上限是绝对时长（1.2s），不再按 min_speech 的倍数卡：
	// 嘈杂环境里 VAD 要 400~600ms 才确认人声，按倍数卡等于把开头吃掉。
	if s.Dialog.PreRollMS > 1200 {
		t.Fatalf("前置缓冲应不超过 1200ms: %d", s.Dialog.PreRollMS)
	}
}

// 唤醒词两侧空白要被去掉，否则 KWS 返回"小黑"时匹配不上"小黑 "。
func TestNormalizeTrimsWakeWords(t *testing.T) {
	s := sample()
	s.Profiles[0].WakeWords = []string{" 小黑 ", "", "   ", "小智"}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := s.Profiles[0].WakeWords
	if len(w) != 2 || w[0] != "小黑" || w[1] != "小智" {
		t.Fatalf("唤醒词未清洗: %#v", w)
	}
}

// Get() 必须返回独立副本：后台页面拿到配置后随手改一个字段（比如规范化数组），
// 不能把服务端正在用的正本改掉。这是典型的"看起来没问题、线上偶发错配置"。
func TestGet_ReturnsIndependentCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	m, err := Open(path, sample())
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}

	cur := m.Get()
	cur.Server.Name = "被改坏了"
	cur.Profiles = append(cur.Profiles, &agent.Profile{ID: "注入"})
	cur.Engine.TTSParams = map[string]string{"url": "被改坏了"}
	cur.Tools.Enabled = append(cur.Tools.Enabled, "注入")

	again := m.Get()
	if again.Server.Name == "被改坏了" {
		t.Fatal("改快照影响了正本")
	}
	if len(again.Profiles) != len(cur.Profiles)-1 {
		t.Fatalf("档案数量被改了: %d", len(again.Profiles))
	}
	if len(again.Engine.TTSParams) != 0 {
		t.Fatal("参数 map 与正本共享了")
	}
	for _, n := range again.Tools.Enabled {
		if n == "注入" {
			t.Fatal("切片与正本共享了")
		}
	}
}

// 回归：把 TTS 供应商关成 none，却还让服务端逐句合成，会导致整轮无声。
// Normalize 必须按配置意图把声音来源一并纠正为设备原生 TTS。
func TestNormalize_NoneTTSProviderForcesDeviceBackend(t *testing.T) {
	s := sample()
	s.Engine.TTSProvider = "none"
	s.Voice.Backend = "local"
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if s.Voice.Backend != "device" {
		t.Fatalf("关掉本地合成后应改用设备 TTS，实际 %q", s.Voice.Backend)
	}

	// 已经显式选了设备 TTS 的，不要被改回去
	s2 := sample()
	s2.Voice.Backend = "device"
	if err := s2.Normalize(); err != nil {
		t.Fatal(err)
	}
	if s2.Voice.Backend != "device" {
		t.Fatalf("本地供应商可用时也应允许设备 TTS，实际 %q", s2.Voice.Backend)
	}

	// 用户切回本地合成，供应商恢复正常
	s3 := sample()
	s3.Engine.TTSProvider = ""
	s3.Voice.Backend = "local"
	if err := s3.Normalize(); err != nil {
		t.Fatal(err)
	}
	if s3.Voice.Backend != "local" {
		t.Fatalf("正常本地合成不该被改写，实际 %q", s3.Voice.Backend)
	}
}

// 老配置把 TTS 参数放在扁平的 tts_params 里。迁移到"每个供应商一份"之后
// 必须清空旧字段，否则两处共存，改一个另一个还在用旧值。
func TestNormalize_MigratesLegacyTTSParams(t *testing.T) {
	s := sample()
	s.Engine.TTSProvider = "doubao"
	s.Engine.TTSParams = map[string]string{"app_id": "app", "access_key": "tok", "voice": ""}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	cfg := s.Engine.TTSProviders["doubao"]
	if cfg["app_id"] != "app" || cfg["access_key"] != "tok" {
		t.Fatalf("参数没迁移过去: %v", s.Engine.TTSProviders)
	}
	if _, ok := cfg["voice"]; ok {
		t.Fatalf("空值不该占键: %v", cfg)
	}
	if s.Engine.TTSParams != nil {
		t.Fatalf("迁移后旧字段应清空: %v", s.Engine.TTSParams)
	}
}

// 新的 tts_providers 已存在时，旧字段不覆盖它（新字段才是事实来源）。
func TestNormalize_DoesNotOverrideExistingProviderCfg(t *testing.T) {
	s := sample()
	s.Engine.TTSProvider = "doubao"
	s.Engine.TTSProviders = map[string]map[string]string{"doubao": {"api_key": "new"}}
	s.Engine.TTSParams = map[string]string{"app_id": "old"}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := s.Engine.TTSProviders["doubao"]["api_key"]; got != "new" {
		t.Fatalf("api_key 被改成了 %q", got)
	}
	if _, ok := s.Engine.TTSProviders["doubao"]["app_id"]; ok {
		t.Fatal("旧参数不该混进已经存在的配置")
	}
}

// 档案的供应商/音色要规整成小写并去空白，否则匹配不上注册名。
func TestNormalize_ProfileVoiceFields(t *testing.T) {
	s := sample()
	s.Profiles[0].TTSProvider = "  DouBao "
	s.Profiles[0].TTSSpeaker = "  zh_female_vv_uranus_bigtts "
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	p := s.Profiles[0]
	if p.TTSProvider != "doubao" {
		t.Fatalf("provider = %q", p.TTSProvider)
	}
	if p.TTSSpeaker != "zh_female_vv_uranus_bigtts" {
		t.Fatalf("speaker = %q", p.TTSSpeaker)
	}
}

// 快照必须是深拷贝：后台改一次"每个供应商的参数"，不能把正在用的配置也改了。
func TestGet_DeepCopiesTTSProviders(t *testing.T) {
	dir := t.TempDir()
	s := sample()
	s.Engine.TTSProviders = map[string]map[string]string{"doubao": {"api_key": "k"}}
	m, err := Open(filepath.Join(dir, "settings.json"), s)
	if err != nil {
		t.Fatal(err)
	}
	snap := m.Get()
	snap.Engine.TTSProviders["doubao"]["api_key"] = "hacked"
	if got := m.Get().Engine.TTSProviders["doubao"]["api_key"]; got != "k" {
		t.Fatalf("快照与正本共享了 map，正本被改成 %q", got)
	}
}
