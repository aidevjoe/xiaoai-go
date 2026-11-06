package dialog

// 固定应答的合成缓存。
//
// 唤醒应答（"我在"/"诶"）、退出语、出错提示这几句是反复出现的同一串字，
// 但每念一次都要走一遍 TTS 合成：本地 8k 模型约 200~400ms，
// 设备云合成要 0.5~1 秒。缓存后从第二次起直接播 PCM。
//
// 几个约束：
//   - 缓存放服务端（数据目录），不占音箱那点 flash
//   - 键里带上文本、采样率、语速、引擎标识：任何一项变了自然失效，
//     不需要额外的"失效通知"机制（改文字必然 miss）
//   - 只缓存"固定应答"这类短句。模型回答内容不重复，缓存只会白占内存。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xiaoai-go/internal/server/audio"
)

// replyCacheMaxRunes 超过这个长度就不缓存了（固定应答都是短句）。
const replyCacheMaxRunes = 40

// ReplyCache 缓存"文本 → PCM"。
type ReplyCache struct {
	dir string

	mu    sync.Mutex
	items map[string]cachedPCM
}

type cachedPCM struct {
	pcm  []byte
	rate int
}

// NewReplyCache 创建缓存。dir 为空时只放内存（重启即失效）。
func NewReplyCache(dir string) *ReplyCache {
	c := &ReplyCache{dir: dir, items: make(map[string]cachedPCM, 16)}
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return c
}

// cacheKey 由文本与合成参数算出键。参数变了键就变，旧文件自然不再命中。
//
// voice 必须进键：同一个"我在"，小美用温柔女声、老张用浑厚男声，
// 只按文本缓存的话第二次唤醒会念出上一个助手的声音。
//
// 采样率刻意不进键：它不是"输入",而是"输出的一部分"——Put 时把真实
// 采样率一起存下来，Get 直接返回，所以同一句话不会因为采样率变化而
// 产生两份缓存。反过来，把 rate 放进去会让"引擎状态串"一变就整批失效。
func cacheKey(text string, speed float32, voiceTag string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%.2f\x00%s", text, speed, voiceTag)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// path 返回磁盘缓存文件路径；dir 为空时返回空串（纯内存）。
func (c *ReplyCache) path(key string) string {
	if c.dir == "" {
		return ""
	}
	return filepath.Join(c.dir, key+".pcm")
}

// Get 读缓存；没有就返回 false。
func (c *ReplyCache) Get(key string) ([]byte, int, bool) {
	c.mu.Lock()
	it, ok := c.items[key]
	c.mu.Unlock()
	if ok {
		return it.pcm, it.rate, true
	}
	p := c.path(key)
	if p == "" {
		return nil, 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return nil, 0, false
	}
	// 磁盘上只存 PCM，采样率返回 0 表示"按当前引擎的采样率"
	c.mu.Lock()
	c.items[key] = cachedPCM{pcm: b}
	c.mu.Unlock()
	return b, 0, true
}

// Put 写入缓存（内存立即生效，磁盘同步落盘，文件很小）。
func (c *ReplyCache) Put(key string, pcm []byte, rate int) {
	if len(pcm) == 0 {
		return
	}
	c.mu.Lock()
	c.items[key] = cachedPCM{pcm: pcm, rate: rate}
	c.mu.Unlock()
	if p := c.path(key); p != "" {
		// 落盘失败只影响"下次启动后的首次合成"，不影响本次
		_ = os.WriteFile(p, pcm, 0o644)
	}
}

// engineSpeed 取当前语音入口的语速（用于缓存键）。取不到按 1.0 算。
func engineSpeed(sp audio.Speaker) float32 {
	if sp == nil {
		return 1
	}
	if v := sp.Speed(); v > 0 {
		return v
	}
	return 1
}

// voiceTag 把音色选择压成一个短标签塞进缓存键。
func voiceTag(v audio.VoiceChoice) string {
	if v.IsZero() {
		return ""
	}
	return v.Provider + "|" + v.Voice
}

// sayCached 播一段固定应答：命中缓存直接播，未命中先合成再存入缓存。
//
// 只有"服务端合成"这条路能缓存：设备原生 TTS 的音频在设备侧生成，
// 服务端拿不到那串字节（要拿到就必须把音频存到音箱上，不划算）。
func (m *Manager) sayCached(ctx context.Context, tgt Target, text string, opt SpeakOptions) error {
	text = strings.TrimSpace(text)
	if text == "" || tgt == nil {
		return nil
	}
	opt = opt.norm()
	sp := m.speaker()

	// 设备原生 TTS、没有合成能力、句子太长 → 走原来的路径
	if opt.DeviceTTS || sp == nil || m.Fixed == nil || runeCount(text) > replyCacheMaxRunes {
		return SayWith(ctx, sp, tgt, text, opt)
	}

	rate := 24000
	key := cacheKey(text, engineSpeed(sp), voiceTag(opt.Voice))
	if pcm, r, ok := m.Fixed.Get(key); ok {
		if r <= 0 {
			r = rate
		}
		return tgt.PlayPCM(ctx, pcm, r)
	}

	pcm, r, err := sp.Synthesize(ctx, text, opt.Voice)
	if err != nil {
		// 合成失败退回正常路径（那边有完整的错误处理与日志）
		return SayWith(ctx, sp, tgt, text, opt)
	}
	if r > 0 {
		rate = r
	}
	m.Fixed.Put(key, pcm, rate)
	return tgt.PlayPCM(ctx, pcm, rate)
}
