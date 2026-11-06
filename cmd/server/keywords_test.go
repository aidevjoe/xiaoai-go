package main

import (
	"strings"
	"testing"

	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/store"
)

func settingsWith(words ...[]string) store.Settings {
	st := store.Defaults()
	st.Profiles = nil
	for i, ws := range words {
		st.Profiles = append(st.Profiles, &agent.Profile{
			ID:        string(rune('a' + i)),
			WakeWords: ws,
			LLM:       agent.LLM{BaseURL: "u", Model: "m"},
		})
	}
	return st
}

func TestKeywordWordsDedupesAndKeepsOrder(t *testing.T) {
	st := settingsWith(
		[]string{"小黑", "你好小黑"},
		[]string{"小智", "小黑", "  你好小黑  "},
		nil,
	)
	words, rev := keywordWords(st)
	want := []string{"小黑", "你好小黑", "小智"}
	if len(words) != len(want) {
		t.Fatalf("去重失败: %#v", words)
	}
	for i := range want {
		if words[i] != want[i] {
			t.Fatalf("顺序或内容不对: %#v", words)
		}
	}
	if rev == "" || len(rev) != 12 {
		t.Fatalf("rev 格式不对: %q", rev)
	}
}

// 版本号只看集合，跟档案顺序无关——否则拖动一下档案就白写一次 flash。
func TestKeywordRevIgnoresOrder(t *testing.T) {
	a := settingsWith([]string{"小黑", "小智"})
	b := settingsWith([]string{"小智"}, []string{"小黑"})
	_, ra := keywordWords(a)
	_, rb := keywordWords(b)
	if ra != rb {
		t.Fatalf("同样的词表应得到相同 rev: %s vs %s", ra, rb)
	}

	c := settingsWith([]string{"小黑", "小智", "翻译"})
	_, rc := keywordWords(c)
	if rc == ra {
		t.Fatal("词表变化后 rev 必须变，否则设备不会重写文件")
	}
}

func TestKeywordWordsEmpty(t *testing.T) {
	words, rev := keywordWords(settingsWith())
	if len(words) != 0 {
		t.Fatalf("没有档案时不该有词: %#v", words)
	}
	if rev == "" {
		t.Fatal("空词表也要有稳定的 rev")
	}
}

// 下发的负载要带上 KWS 阈值，设备侧将来调参不用再单独请求。
func TestKWSPayloadCarriesThreshold(t *testing.T) {
	st := settingsWith([]string{"小黑"})
	st.Engine.KWSScore = 3.5
	st.Engine.KWSThreshold = 0.4

	s := &server{store: mustStore(t, st)}
	p := s.kwsPayload(st)
	if len(p.Words) != 1 || p.Words[0] != "小黑" {
		t.Fatalf("词表不对: %#v", p.Words)
	}
	if p.Score != 3.5 || p.Threshold != 0.4 {
		t.Fatalf("阈值没带上: %+v", p)
	}
	if !strings.HasPrefix(p.Rev, keywordRevOf(st)) {
		t.Fatalf("rev 不一致: %s", p.Rev)
	}
}

func keywordRevOf(st store.Settings) string {
	_, rev := keywordWords(st)
	return rev
}

func mustStore(t *testing.T, st store.Settings) *store.Manager {
	t.Helper()
	m, err := store.Open(t.TempDir()+"/settings.json", st)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
