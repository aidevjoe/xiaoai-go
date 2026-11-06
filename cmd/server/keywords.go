package main

// 唤醒词同步：服务端是唯一事实来源，设备只负责把清单落到本地文件。
//
// 为什么需要它：唤醒词配在助手档案里（跟着 LLM 走），而文件在音箱上。
// 不打通就只能登设备手改，改完两边还容易对不上——「叫得应但认错档案」
// 这类问题基本都出在这里。

import (
	"context"
	"hash/fnv"
	"log"
	"sort"
	"strings"
	"time"

	"xiaoai-go/internal/proto"
	"xiaoai-go/internal/server/store"
)

// kwsState 是一台设备最近一次同步的结果。
type kwsState struct {
	Rev   string    `json:"rev"`
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Err   string    `json:"err,omitempty"`
	Count int       `json:"count"`
	Path  string    `json:"path,omitempty"`
	Skip  bool      `json:"skip,omitempty"`
}

// keywordWords 汇总全部档案的唤醒词：去重、保持声明顺序、顺带算版本号。
//
// rev 用内容哈希：设备据此判断要不要重写文件，
// 避免每次重连都写一遍 flash（音箱的 /data 只有 8MB 可用，写入寿命也有限）。
func keywordWords(st store.Settings) ([]string, string) {
	seen := make(map[string]bool, 8)
	words := make([]string, 0, 8)
	for _, p := range st.Profiles {
		if p == nil {
			continue
		}
		for _, w := range p.WakeWords {
			w = strings.TrimSpace(w)
			if w == "" || seen[w] {
				continue
			}
			seen[w] = true
			words = append(words, w)
		}
	}
	sorted := append([]string(nil), words...)
	sort.Strings(sorted) // 哈希只跟集合有关，与顺序无关
	h := fnv.New64a()
	for _, w := range sorted {
		h.Write([]byte(w))
		h.Write([]byte{0})
	}
	return words, formatRev(h.Sum64())
}

// formatRev 把哈希写成短十六进制，便于在日志和界面上看。
func formatRev(v uint64) string {
	const hex = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = hex[v&0xf]
		v >>= 4
	}
	return string(b[:12])
}

// kwsPayload 按当前配置构造下发负载。
func (s *server) kwsPayload(st store.Settings) proto.KWSSyncPayload {
	words, rev := keywordWords(st)
	return proto.KWSSyncPayload{
		Words:     words,
		Rev:       rev,
		Score:     st.Engine.KWSScore,
		Threshold: st.Engine.KWSThreshold,
	}
}

// syncKeywords 把当前唤醒词推给设备；id 为空表示推给全部在线设备。
//
// 失败不返回错误：设备离线、超时都是常态，状态记下来给后台看即可，
// 下一次重连会自动重试。
func (s *server) syncKeywords(ctx context.Context, id string) map[string]kwsState {
	out := map[string]kwsState{}
	if s.store == nil {
		return out
	}
	payload := s.kwsPayload(s.store.Get())

	sessions := s.h.List()
	for _, sess := range sessions {
		if id != "" && sess.ID != id {
			continue
		}
		st := s.pushKeywords(ctx, sess.ID, payload)
		out[sess.ID] = st
		s.kwsMu.Lock()
		s.kws[sess.ID] = st
		s.kwsMu.Unlock()
	}
	return out
}

// pushKeywords 对单台设备发一次同步请求。
func (s *server) pushKeywords(ctx context.Context, deviceID string, payload proto.KWSSyncPayload) kwsState {
	sess, err := s.h.Get(deviceID)
	if err != nil {
		return kwsState{Rev: payload.Rev, At: time.Now(), Err: "设备离线"}
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := sess.Request(reqCtx, proto.CmdKWSSync, payload, 4*time.Second)
	if err != nil {
		log.Printf("[%s] 唤醒词同步失败: %v", deviceID, err)
		return kwsState{Rev: payload.Rev, At: time.Now(), Err: err.Error()}
	}
	var res proto.KWSSyncResult
	_ = resp.Decode(&res)
	if !res.OK {
		msg := res.Message
		if msg == "" {
			msg = "设备返回失败"
		}
		return kwsState{Rev: payload.Rev, At: time.Now(), Err: msg}
	}
	log.Printf("[%s] 唤醒词已同步: %d 个 rev=%s → %s", deviceID, res.Count, res.Rev, res.Path)
	return kwsState{
		Rev: res.Rev, At: time.Now(), OK: true,
		Count: res.Count, Path: res.Path, Skip: res.Skip,
	}
}

// kwsStatus 返回当前清单与各设备的同步状态（后台展示用）。
func (s *server) kwsStatus() map[string]any {
	words, rev := keywordWords(s.config())
	type devState struct {
		ID     string `json:"id"`
		Online bool   `json:"online"`
		kwsState
	}
	devs := []devState{}
	seen := map[string]bool{}
	for _, sess := range s.h.List() {
		seen[sess.ID] = true
		s.kwsMu.Lock()
		st := s.kws[sess.ID]
		s.kwsMu.Unlock()
		if st.Rev == "" {
			st.Rev = "(未同步)"
		}
		devs = append(devs, devState{ID: sess.ID, Online: true, kwsState: st})
	}
	// 离线设备也列出来，否则"上次同步过"的记录会凭空消失
	s.kwsMu.Lock()
	for id, st := range s.kws {
		if !seen[id] {
			devs = append(devs, devState{ID: id, Online: false, kwsState: st})
		}
	}
	s.kwsMu.Unlock()
	sort.Slice(devs, func(i, j int) bool { return devs[i].ID < devs[j].ID })

	return map[string]any{
		"words":   words,
		"rev":     rev,
		"count":   len(words),
		"devices": devs,
	}
}
