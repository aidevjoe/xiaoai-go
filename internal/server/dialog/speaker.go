// Package dialog 把一个设备的一轮对话串起来：
// 唤醒（选档案）→ 识别文本 → LLM 流式 → 句子切分 → 逐句合成 → 推到音箱。
package dialog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"xiaoai-go/internal/server/audio"
)

// Target 是播放目标（由设备会话实现）。
type Target interface {
	// PlayPCM 把 PCM 推给设备播放。rate 为采样率，需与设备侧 aplay 一致。
	PlayPCM(ctx context.Context, pcm []byte, rate int) error
}

// TextSpeaker 是可选的"让设备自己念"能力。
//
// 设备用自家云端 TTS 念，音色自然得多；代价是语速不可调和依赖网络，
// 所以做成可选接口：实现不了就自动退回本地合成。
type TextSpeaker interface {
	SayText(ctx context.Context, text string) error
}

// DefaultMaxSentenceRunes 单句上限，超过就硬切，保证首句尽快合成。
const DefaultMaxSentenceRunes = 20

// SpeakOptions 是播报行为的可调参数（后台可改）。
// TextSpeakerEx 支持同时指定"不要收尾音"和"不要闭麦"。
type TextSpeakerEx interface {
	SayTextEx(ctx context.Context, text string, noDone, noMute bool) error
}

// TextSpeakerQuiet 是"播完不要收尾提示音"的 Target（收尾语用）。
type TextSpeakerQuiet interface {
	SayTextQuiet(ctx context.Context, text string) error
}

type SpeakOptions struct {
	// MaxSentenceRunes 单句字数上限，越小首字越快、但语气越碎。
	MaxSentenceRunes int
	// MaxSentences 单轮最多播几句，0 或负数表示不限。防止模型喋喋不休。
	MaxSentences int
	// StripEmoji 是否剥掉表情与 markdown 符号，避免 TTS 念出怪音。
	StripEmoji bool
	// DeviceTTS 为 true 时把文字交给设备原生 TTS，而不是本地合成。
	DeviceTTS bool
	// NoDone 表示这段话是收尾语（退出应答等），
	// 播完不要加"说完了"提示音——用户已经听到告别语，再加一声反而混淆。
	NoDone bool
	// NoMute 表示播这句话时不闭麦。用于唤醒应答这种一两声的短句：
	// 为它闭麦会把用户紧接着说的第一个词吞掉，而它自己的回声只是一声
	// "诶"，服务端剥掉开头的语气词就行。
	NoMute bool
	// Voice 是这句话用哪个平台/音色/语速念。空值一律跟随全局。
	// 由 Manager.speakOptionsOf 合并"全局 → 档案覆盖"后填入。
	Voice audio.VoiceChoice
}

// DefaultSpeakOptions 返回实测下来的默认值。
func DefaultSpeakOptions() SpeakOptions {
	return SpeakOptions{MaxSentenceRunes: DefaultMaxSentenceRunes, MaxSentences: 6, StripEmoji: true}
}

// norm 补齐零值。
func (o SpeakOptions) norm() SpeakOptions {
	if o.MaxSentenceRunes <= 0 {
		o.MaxSentenceRunes = DefaultMaxSentenceRunes
	}
	return o
}

// text 按选项清理要朗读的文本。
func (o SpeakOptions) text(s string) string {
	s = strings.TrimSpace(s)
	if o.StripEmoji {
		s = audio.Sanitize(s)
	}
	return s
}

// StreamSpeaker 边收 LLM 增量边合成播放。
//
// 关键：不等全文结束，第一句一完整就送去 TTS，这样首字延迟
// 只等于"首句合成时间"，而不是"全文生成 + 全文合成"。
//
// 合成走 audio.Speaker（而不是具体引擎）：这样"全局一个本地模型"和
// "按档案切到豆包/Edge"对播放器完全透明，换供应商不用改这里一行。
type StreamSpeaker struct {
	sp  audio.Speaker
	tgt Target
	opt SpeakOptions

	mu  sync.Mutex
	buf strings.Builder

	sentences chan string
	done      chan struct{}
	errOnce   sync.Once
	err       error
	spoken    int // 已送出的句子数

	// onFirstAudio 在第一段音频下发成功后回调一次（后台统计"多久听到声音"）
	onFirstAudio func()
	firstOnce    sync.Once
}

// NewStreamSpeaker 启动后台合成协程。
// sp 为 nil 时不合成（本机开发只跑 LLM/对话链路时会这样），文本仍会累积。
func NewStreamSpeaker(ctx context.Context, sp audio.Speaker, tgt Target) *StreamSpeaker {
	return NewStreamSpeakerWith(ctx, sp, tgt, DefaultSpeakOptions())
}

// NewStreamSpeakerWith 用自定义播报参数启动。
func NewStreamSpeakerWith(ctx context.Context, sp audio.Speaker, tgt Target, opt SpeakOptions) *StreamSpeaker {
	opt = opt.norm()
	s := &StreamSpeaker{
		sp:        sp,
		tgt:       tgt,
		opt:       opt,
		sentences: make(chan string, 8),
		done:      make(chan struct{}),
	}
	go s.worker(ctx)
	return s
}

// Write 追加一段模型输出，内部按句切分。
func (s *StreamSpeaker) Write(delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	s.buf.WriteString(delta)
	for {
		text := s.buf.String()
		// 首句用更短的上限：出声时间只取决于第一句的合成耗时，
		// 把它压短，用户能明显更早听到回应；后续句子仍用完整上限，避免语气太碎。
		limit := s.opt.MaxSentenceRunes
		if s.spoken == 0 {
			limit = firstLimit(limit)
		}
		cut, tail := splitFirst(text, limit)
		if cut == "" {
			break
		}
		s.buf.Reset()
		s.buf.WriteString(tail)
		s.mu.Unlock()
		s.emit(cut)
		s.mu.Lock()
	}
	s.mu.Unlock()
}

// Close 冲掉剩余文本并等合成播放完成。
func (s *StreamSpeaker) Close() error {
	s.mu.Lock()
	rest := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	s.mu.Unlock()
	if rest != "" {
		s.emit(rest)
	}
	close(s.sentences)
	<-s.done
	return s.err
}

func (s *StreamSpeaker) emit(raw string) {
	text := s.opt.text(raw)
	// 纯标点段（模型把 "。" 单独作为一个增量吐出来时会发生）不值得单独
	// 合成一次：设备 TTS 会为它生成一个自带前后静音的独立音频，白白多出 1~2 秒。
	if text == "" || !hasSpeakable(text) {
		return
	}
	// 切句可能正好落在两个英文词之间的空格上。保留这个空格，
	// 否则设备 TTS 把相邻两段拼成一句时 "apple pie" 会粘成 "applepie"。
	if strings.HasSuffix(raw, " ") && !strings.HasSuffix(text, " ") {
		text += " "
	}
	s.mu.Lock()
	if s.opt.MaxSentences > 0 && s.spoken >= s.opt.MaxSentences {
		s.mu.Unlock()
		return // 说够了就闭嘴，别把用户的耐心念完
	}
	s.spoken++
	s.mu.Unlock()
	s.sentences <- text
}

// 设备原生 TTS 两次下发之间的两个阈值（按字符数）。
//
// firstChunkRunes 决定"它多久开口"：28 字时模型要多生成约 1 秒，
// 实测首字到开口的空档能到 500ms（后台"开口差"指标）。降到 12 字后
// 开口明显提前，代价是短回答可能多一次云合成往返。
//
// maxPendingRunes 决定"后续多久一送"。
//
// 设备 TTS 每多一段就多一次独立合成 + 一次播放器启动，段间必然有一道缝，
// 所以长回答要尽量少分段（32 字一段）。短回答靠 splitFirst 的词边界保护
// 会自动合成一整段下发，压根不会走到这里。
// 真出现多段时，客户端在入队瞬间就预合成下一段，把合成耗时藏进上一段的播放里。
const (
	firstChunkRunes = 12
	maxPendingRunes = 32
	// deviceMergeWait 是设备原生 TTS 首段发出前的"合段等待"：
	// 首段攒够 firstChunkRunes 后再等这么久，模型若在这期间把剩下的字吐完，
	// 就并成一段下发。400ms 大于常见的流式间隔（<200ms），又小到不影响开口速度。
	deviceMergeWait = 400 * time.Millisecond
)

// runeCount 按字符（不是字节）计数，中文一个字算一个。
func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// hasSpeakable 判断这段文本里有没有真正能念出来的内容。
// 用 unicode 分类而不是"非 ASCII"：全角标点「。」也是非 ASCII，但它念不出来。
func hasSpeakable(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// Spoken 返回已送出的句子数（后台指标用）。
func (s *StreamSpeaker) Spoken() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spoken
}

func (s *StreamSpeaker) worker(ctx context.Context) {
	defer close(s.done)

	// 设备原生 TTS 的特殊处理：不要按句发。
	//
	// 每调用一次 tts_play.sh 都是一次云端合成往返（实测 1~3 秒），
	// 按句发就是"一句一停"——用户听到的是"我是 Kimi，… 停 2 秒 … 月之暗面开发的…"。
	//
	// 但也不能等整段都生成完再发：模型首字之后还要好几秒才吐完，
	// 那样"它开始说话"的时间会被推迟到最后。所以取两段式：
	//   1) 攒够一小段（约一两句）就先播，让用户尽快听到声音
	//   2) 剩下的全部攒到最后一次发，整轮只有一次句间空档
	if s.opt.DeviceTTS {
		if ts, ok := s.tgt.(TextSpeaker); ok {
			var pending strings.Builder
			sentAny := false

			say := func(text string) {
				text = strings.TrimSpace(text)
				if text == "" {
					return
				}
				sentAny = true
				if err := ts.SayText(ctx, text); err != nil {
					s.fail(fmt.Errorf("设备播报失败: %w", err))
					return
				}
				if s.onFirstAudio != nil {
					s.firstOnce.Do(s.onFirstAudio)
				}
			}

			// 短回答的"合段等待"：首段攒够 firstChunkRunes 后先别急着发，
			// 等模型 mergeWait 这么久——大多数短回答（"香蕉的英文是 banana，
			// 复数就是 bananas。"）后面几个字马上就到，等一下就能整句一段下发。
			// 分两段发的问题不在网络衔接（实测两段是首尾相接播的），而在云端
			// 给每段独立合成：段尾的逗号被念成句末长停顿，段首又自带静音，
			// 两个一叠加就是用户听到的"逗号之后停了两秒"。
			merge := time.NewTimer(time.Hour)
			merge.Stop()
			armed := false
			stopMerge := func() {
				if !armed {
					return
				}
				armed = false
				if !merge.Stop() {
					select {
					case <-merge.C:
					default:
					}
				}
			}

			for {
				select {
				case text, ok := <-s.sentences:
					if !ok {
						stopMerge()
						say(pending.String())
						return
					}
					if err := ctx.Err(); err != nil {
						s.fail(err)
						continue // 排空通道，避免 Close 卡住
					}
					pending.WriteString(text)
					n := runeCount(pending.String())
					// 长回答：攒满一大段立刻发，别拖开口速度
					if n >= maxPendingRunes {
						stopMerge()
						say(pending.String())
						pending.Reset()
						continue
					}
					// 首段之后没有再等的价值（缝隙已被上一段的播放盖住），
					// 剩下的等 Close 一起发
					if sentAny {
						continue
					}
					if n >= firstChunkRunes {
						if armed {
							merge.Stop()
						}
						merge.Reset(deviceMergeWait)
						armed = true
					}
				case <-merge.C:
					armed = false
					say(pending.String())
					pending.Reset()
				}
			}
		}
	}

	// 本地合成：边播边合成下一句，消掉句间空档
	s.playPipelined(ctx)
}

// playPipelined 边播边合成：播放第 N 句时，第 N+1 句已经在合成了。
//
// 原来的写法是"合成 → 播 → 合成 → 播"串行，句间必然会空出一次合成的时间
// （本地 8k 模型 200~400ms，足够听出来是"一句一停"）。
// 合成和播放互不依赖，把合成挪到一个协程里就能把这段空档吃掉。
//
// 采样率必须按"每一段音频自己的"来播：同一次回答里换了音色/平台会导致
// 采样率不同（本地 8k、豆包 24k、Edge 24k），用建播放器那一刻的值会播成怪声。
func (s *StreamSpeaker) playPipelined(ctx context.Context) {
	if s.sp == nil || s.tgt == nil {
		for range s.sentences { // 无合成能力：只排空
		}
		return
	}

	type result struct {
		pcm  []byte
		rate int
		err  error
	}
	jobs := make(chan string, 4)
	outs := make(chan result, 2)

	// 句子 → 合成队列
	go func() {
		defer close(jobs)
		for text := range s.sentences {
			if ctx.Err() != nil {
				continue // 继续排空，别让 Close 卡住
			}
			jobs <- text
		}
	}()
	// 合成（独立协程，永远比播放快一步）
	go func() {
		defer close(outs)
		for text := range jobs {
			pcm, rate, err := s.sp.Synthesize(ctx, text, s.opt.Voice)
			outs <- result{pcm: pcm, rate: rate, err: err}
		}
	}()

	for r := range outs {
		if r.err != nil {
			s.fail(fmt.Errorf("合成失败: %w", r.err))
			continue
		}
		if err := s.tgt.PlayPCM(ctx, r.pcm, r.rate); err != nil {
			s.fail(fmt.Errorf("播放失败: %w", err))
			continue
		}
		if s.onFirstAudio != nil {
			s.firstOnce.Do(s.onFirstAudio)
		}
	}
}

func (s *StreamSpeaker) fail(err error) {
	s.errOnce.Do(func() { s.err = err })
}

// firstLimit 返回首句的字数上限：取完整上限的 3/5，最少 8 个字。
//
// 20 字的句子模型要生成 1~1.5 秒，而 12 字只要 0.6~0.9 秒——
// 对"等多久才出声"来说这是最直接的一刀。
func firstLimit(maxRunes int) int {
	n := maxRunes * 3 / 5
	if n < 8 {
		n = 8
	}
	if n > maxRunes {
		n = maxRunes
	}
	return n
}

// splitFirst 从文本里切出第一个可播报的句子。
// 返回 (句子, 剩余)；不足一句返回 ("", 原文)。
func splitFirst(text string, maxRunes int) (string, string) {
	if maxRunes <= 0 {
		maxRunes = DefaultMaxSentenceRunes
	}
	rs := []rune(text)
	for i, r := range rs {
		if !isBoundary(r) {
			continue
		}
		// "3.14" 里的小数点不是句末；缩写（e.g. / Mr.）同理不当断句点
		if r == '.' && i > 0 && i+1 < len(rs) && isDigit(rs[i-1]) && isDigit(rs[i+1]) {
			continue
		}
		// 连续的 ？！ 归到同一句
		j := i
		for j+1 < len(rs) && isBoundary(rs[j+1]) {
			j++
		}
		return string(rs[:j+1]), string(rs[j+1:])
	}
	if len(rs) >= maxRunes {
		// 太长还没标点：退到最后一个逗号，没有就硬切
		for i := len(rs) - 1; i > 0; i-- {
			if rs[i] == '，' || rs[i] == ',' || rs[i] == '、' {
				return string(rs[:i+1]), string(rs[i+1:])
			}
		}
		// 硬切绝不能落在英文单词或数字串中间。
		// 实测事故：模型先吐 "香蕉的英文还是 banana" 再吐 "。"，
		// 首句上限 12 字把 banana 劈成 "bana" + "na。"；设备 TTS 把两段
		// 各自合成、各自起一个播放器，听起来就成了 "banna … a"，中间还空一截。
		cut := maxRunes
		if cut > len(rs) {
			cut = len(rs)
		}
		if cut < len(rs) && isWordRune(rs[cut-1]) && isWordRune(rs[cut]) {
			wordStart := cut
			for wordStart > 0 && isWordRune(rs[wordStart-1]) {
				wordStart--
			}
			if wordStart > 0 {
				// 退到词首：把整个单词留给下一段
				cut = wordStart
			} else {
				// 整段开头就是一个超长单词：扩到词尾，别死等标点
				for cut < len(rs) && isWordRune(rs[cut]) {
					cut++
				}
			}
		}
		return string(rs[:cut]), string(rs[cut:])
	}
	return "", text
}

func isBoundary(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '…', '!', '?', ';', '.', '\n':
		return true
	}
	return false
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// isWordRune 判断这个字符是否属于"不能从中间劈开"的英文单词/数字。
// 连字符与撇号也算词内字符（well-known / don't）。
func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || isDigit(r) ||
		r == '-' || r == '\'' || r == '_'
}

// Say 一次性播报（不经过模型），用于唤醒应答、退出语、错误提示。
func Say(ctx context.Context, sp audio.Speaker, tgt Target, text string) error {
	return SayWith(ctx, sp, tgt, text, DefaultSpeakOptions())
}

// SayWith 用自定义参数一次性播报。
func SayWith(ctx context.Context, sp audio.Speaker, tgt Target, text string, opt SpeakOptions) error {
	if tgt == nil {
		return nil
	}
	opt = opt.norm()
	text = opt.text(text)
	if text == "" {
		return nil
	}
	if opt.DeviceTTS {
		if ex, ok := tgt.(TextSpeakerEx); ok && (opt.NoDone || opt.NoMute) {
			return ex.SayTextEx(ctx, text, opt.NoDone, opt.NoMute)
		}
		if ts, ok := tgt.(TextSpeaker); ok {
			if opt.NoDone {
				if o, ok := tgt.(TextSpeakerQuiet); ok {
					return o.SayTextQuiet(ctx, text)
				}
			}
			return ts.SayText(ctx, text)
		}
	}
	if sp == nil {
		return nil
	}
	for _, s := range audio.SplitSentences(text, opt.MaxSentenceRunes) {
		pcm, rate, err := sp.Synthesize(ctx, s, opt.Voice)
		if err != nil {
			return err
		}
		if err := tgt.PlayPCM(ctx, pcm, rate); err != nil {
			return err
		}
	}
	return nil
}

// ErrBusy 表示设备正在说话，拒绝新的请求。
var ErrBusy = errors.New("dialog: 设备正忙")

// 状态机取值。
const (
	StateIdle      = "idle"
	StateListening = "listening"
	StateThinking  = "thinking"
	StateSpeaking  = "speaking"
)

// now 便于测试替换。
var now = time.Now
