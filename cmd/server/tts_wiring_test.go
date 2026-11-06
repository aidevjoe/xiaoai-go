package main

import (
	"io"
	"log"
	"testing"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
	"xiaoai-go/internal/server/dialog"
	"xiaoai-go/internal/server/store"
)

// 这条链是"档案音色真的生效"的关键，任何一环断了都表现为
// "后台选了音色，喊它还是老声音"，而且不报错——所以单独测一遍。
func TestApplyDialogConfig_WiresGlobalVoiceAndSpeech(t *testing.T) {
	reg, err := agent.NewRegistry([]*agent.Profile{{
		ID: "p", Name: "管家", WakeWords: []string{"小黑"},
		LLM: agent.LLM{BaseURL: "http://127.0.0.1:1", Model: "m"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc := audio.NewService("doubao", map[string]string{"api_key": "k"})
	mgr := dialog.NewManager(reg, nil, nil, log.New(io.Discard, "", 0))
	s := &server{dm: mgr, speech: svc}

	st := store.Defaults()
	st.Profiles = []*agent.Profile{{
		ID: "p", Name: "管家", WakeWords: []string{"小黑"},
		LLM: agent.LLM{BaseURL: "http://127.0.0.1:1", Model: "m"},
	}}
	st.Engine.TTSProvider = "doubao"
	st.Engine.TTSVoice = "zh_female_vv_uranus_bigtts"
	st.Engine.TTSProviders = map[string]map[string]string{"doubao": {"api_key": "k"}}
	st.Voice.Speed = 1.1

	s.applyDialogConfig(st)

	if mgr.GlobalVoice.Provider != "doubao" {
		t.Fatalf("全局供应商没灌进去: %+v", mgr.GlobalVoice)
	}
	if mgr.GlobalVoice.Voice != "zh_female_vv_uranus_bigtts" {
		t.Fatalf("全局音色没灌进去: %+v", mgr.GlobalVoice)
	}
	if mgr.GlobalVoice.Params["api_key"] != "k" {
		t.Fatalf("平台参数没灌进去: %+v", mgr.GlobalVoice)
	}
	// 语速必须单独落到路由层：固定应答走的是 Speaker.SetSpeed 那条兜底路径
	if got := svc.Speed(); got != 1.1 {
		t.Fatalf("路由层语速 = %v，期望 1.1", got)
	}
	// 语音入口必须被换成路由层，否则档案音色不生效
	if mgr.Speech == nil {
		t.Fatal("Speech 没注入")
	}
	if _, ok := mgr.Speech.(*audio.Service); !ok {
		t.Fatalf("Speech 应是 *audio.Service，实际 %T", mgr.Speech)
	}
}

// 注入路由层之后，重载引擎不能再把 Speech 顶回 EngineSpeaker，
// 否则"重载一次引擎，所有助手的音色全变回同一个"。
func TestSetEngine_DoesNotClobberInjectedService(t *testing.T) {
	reg, err := agent.NewRegistry([]*agent.Profile{{
		ID: "p", Name: "管家", WakeWords: []string{"小黑"},
		LLM: agent.LLM{BaseURL: "http://127.0.0.1:1", Model: "m"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc := audio.NewService("doubao", nil)
	mgr := dialog.NewManager(reg, nil, nil, log.New(io.Discard, "", 0))
	mgr.SetSpeech(svc)

	mgr.SetEngine(audio.NewMockEngine(0.9, "x"))

	if _, ok := mgr.Speech.(*audio.Service); !ok {
		t.Fatalf("重载引擎后 Speech 被顶掉了: %T", mgr.Speech)
	}
}

// 没注入路由层时（测试/单模型场景），SetEngine 仍然要能更新合成入口。
func TestSetEngine_UpdatesEngineSpeakerWhenNotInjected(t *testing.T) {
	reg, err := agent.NewRegistry([]*agent.Profile{{
		ID: "p", Name: "管家", WakeWords: []string{"小黑"},
		LLM: agent.LLM{BaseURL: "http://127.0.0.1:1", Model: "m"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mgr := dialog.NewManager(reg, nil, nil, log.New(io.Discard, "", 0))
	eng := audio.NewMockEngine(0.9, "x")
	mgr.SetEngine(eng)
	if !mgr.Speech.Available() {
		t.Fatal("换上引擎后应可用")
	}
}
