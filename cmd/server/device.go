package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"

	"xiaoai-go/internal/server/agent"

	"xiaoai-go/internal/proto"
	"xiaoai-go/internal/server/audio"
	"xiaoai-go/internal/server/dialog"
	"xiaoai-go/internal/server/hub"
)

// deviceOptions 是建管道时用的配置快照（后台改了就重建管道）。
type deviceOptions struct {
	MinSpeech    time.Duration
	MinSilence   time.Duration
	PreRoll      time.Duration
	MaxUtterance time.Duration
	AudioDir     string // 非空则把每句话的原始录音落盘（调试用）
}

// deviceAudio 把一台设备的音频帧接进 VAD/ASR 流水线。
type deviceAudio struct {
	id  string
	hub *hub.Hub
	eng audio.Engine
	dm  *dialog.Manager

	pipe *audio.Pipeline
	ch   chan []byte
	opt  deviceOptions
	// freeTalk 返回是否开启免唤醒模式（每次判断，改了配置立刻生效）
	freeTalk func() bool
	// muted 返回当前是否该闭麦（音箱正在播报时不听，否则会听见自己）
	muted func() bool
	// ackTone 返回是否要"说完叮一声"
	ackTone func() bool
	// wakeByASR 返回是否启用"识别文本唤醒"
	wakeByASR func() bool
	// wakeWords 返回当前全部唤醒词
	wakeWords func() []string
	// release 归还建管道时从引擎句柄借的那份引用（stop 时调用）
	release func()

	lastPCM []byte // 最近一句话的原始音频（OpenText 里取出来落盘）

	// cancel/doneCh 用来真正停掉这个管道的消费协程。
	// 旧实现直接把 map 里那一项删掉，协程还挂在 ctx.Background() 上永不退出：
	// 每次重载配置就泄漏一条协程 + 一个引擎引用，还继续吃 ASR/TTS 的锁。
	cancel context.CancelFunc
	doneCh chan struct{}

	mu   sync.Mutex
	busy bool // 正在跑模型/TTS，期间丢弃新帧避免堆积
	// wakeAt 是最近一次 KWS 命中的时刻（用来量"从喊到开口"到底花在哪）
	wakeAt time.Time
	// ackAt 是最近一次"唤醒瞬间就叮过"的时刻。
	// 它用来避免 2 秒后出文本时再叮一次（两声听着像卡了两下）。
	ackAt time.Time
	// lastFrameAt 是最近一帧音频到达的时刻：文本回来的时间减它 ≈ ASR 推理耗时
	lastFrameAt time.Time
}

func newDeviceAudio(id string, h *hub.Hub, eng audio.Engine, release func(), dm *dialog.Manager, opt deviceOptions,
	freeTalk, muted, ackTone, wakeByASR func() bool, wakeWords func() []string) *deviceAudio {
	if release == nil {
		release = func() {}
	}
	d := &deviceAudio{
		id: id, hub: h, eng: eng, dm: dm, ch: make(chan []byte, 64), opt: opt,
		freeTalk: freeTalk, muted: muted, ackTone: ackTone,
		wakeByASR: wakeByASR, wakeWords: wakeWords, release: release,
	}
	if eng != nil {
		seg := audio.NewSegmenterWith(opt.MinSpeech, opt.MinSilence, opt.PreRoll, opt.MaxUtterance)
		// onText 与 Push 在同一个协程里同步回调，取一下原始音频用于落盘
		d.pipe = audio.NewPipelineWith(eng, seg, func(text string, pcm []byte) {
			d.lastPCM = pcm
		})
	}
	return d
}

// start 起消费协程。返回后 cancel/doneCh 一定非空，stop() 才能安全调用。
func (d *deviceAudio) start(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.doneCh = make(chan struct{})
	go func() {
		defer close(d.doneCh)
		// 引用在协程真正退出时才还：卡在推理里被 stop 放弃等待时，
		// 宁可让旧引擎多活一会儿，也不能在它还在跑的时候把 C 对象关掉。
		defer d.dropRef()
		d.Run(ctx)
	}()
}

// dropRef 归还引擎引用（只生效一次）。
func (d *deviceAudio) dropRef() {
	d.mu.Lock()
	r := d.release
	d.release = nil
	d.mu.Unlock()
	if r != nil {
		r()
	}
}

// stop 停掉消费协程并等它退出。超时只记日志、不强杀：
// 卡在模型推理里的协程没法从外面打断，引用由协程退出时自己归还。
func (d *deviceAudio) stop(timeout time.Duration) {
	if d == nil {
		return
	}
	if d.cancel != nil {
		d.cancel()
	}
	if d.doneCh == nil {
		d.dropRef()
		return
	}
	select {
	case <-d.doneCh:
	case <-time.After(timeout):
		log.Printf("[%s] 音频管道 %v 内未退出，放弃等待（引擎引用等它退出时归还）", d.id, timeout)
	}
}

// Push 投递一帧音频（不阻塞读循环）。
func (d *deviceAudio) Push(pcm []byte) {
	if d.pipe == nil {
		return
	}
	d.mu.Lock()
	busy := d.busy
	d.mu.Unlock()
	if busy {
		return // 模型思考/播报期间不听，防止把 TTS 自己的声音识别进去
	}
	// 音箱正在出声（含播完的尾巴）也闭麦：PlayPCM 返回时 aplay 还在播，
	// 不挡住就会把自己的话当成用户输入。
	if d.muted != nil && d.muted() {
		return
	}
	select {
	case d.ch <- pcm:
	default: // 队列满就丢，宁可丢帧也不能阻塞网络读
	}
}

// Run 消费音频帧：VAD 分段 → ASR → 对话。
func (d *deviceAudio) Run(ctx context.Context) {
	if d.pipe == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case pcm, ok := <-d.ch:
			if !ok {
				return
			}
			// 先过一遍 KWS：命中就在音频域立刻唤醒，
			// 不用等这句说完、也不用等 ASR 出文本（那条路要多花 1~2 秒）
			d.detectWake(ctx, pcm)
			d.lastFrameAt = time.Now()
			text, err := d.pipe.Push(ctx, pcm)
			if err != nil {
				log.Printf("[%s] 识别失败: %v", d.id, err)
				continue
			}
			if text == "" {
				continue
			}
			d.handle(ctx, text)
			d.lastPCM = nil
		}
	}
}

// detectWake 用 KWS 模型在当前帧里找唤醒词。
//
// 命中后立刻应答（"我在"），但**不**重置分段器：
// 用户往往是一句话说完的（"你好小黑，今天天气怎么样"），
// 后面的提问要留在管道里，交给 ASR 出文本，再由 handle 剥掉开头的唤醒词。
func (d *deviceAudio) detectWake(ctx context.Context, pcm []byte) {
	if d.eng == nil || d.dm == nil || d.hub == nil {
		return
	}
	kw, err := d.eng.DetectKeyword(pcm)
	if err != nil || kw == "" {
		return
	}
	sess, err := d.hub.Get(d.id)
	if err != nil {
		return
	}
	dlg := d.dm.Session(d.id, &deviceTarget{hub: d.hub, id: d.id, sess: sess})
	if d.freeTalk != nil && d.freeTalk() {
		// 免唤醒模式本来就会应答每一句，这里只记一笔
		log.Printf("[%s] KWS 命中「%s」（免唤醒模式，无需应答）", d.id, kw)
		_ = dlg.WakeSilent(ctx, kw)
		return
	}
	d.mu.Lock()
	d.wakeAt = time.Now()
	d.mu.Unlock()
	log.Printf("[%s] KWS 唤醒命中「%s」（对话中=%v）", d.id, kw, dlg.Active())
	// 只把对话"点亮"，先不出声。
	//
	// 为什么不在这一刻就念"我在"：用户经常一口气说完
	//（"你好小黑，今天星期几"），而客户端在播报期间是闭麦的，
	// 这一秒正好把后面的提问吞掉——实测就是"完全没反应"。
	// 所以应答语延到"确认用户说完了"再决定播不播：
	//   - 只说了唤醒词 → 播"我在"（见 handle 里 rest=="" 的分支）
	//   - 唤醒词后面还有话 → 直接回答，不插话
	// 命中这一刻立刻"叮"一声：这是本地提示音，几十毫秒就出声。
	// 用户体感上的"它听见了"就靠这一声——后面的识别和应答要 1.5~2.5 秒，
	// 那些时间里如果完全静默，用户会觉得它没反应（小爱秒回靠的也是这招）。
	if d.ackTone != nil && d.ackTone() {
		d.mu.Lock()
		d.ackAt = time.Now()
		d.mu.Unlock()
		d.playAckTone(ctx)
		log.Printf("[%s] 唤醒即「叮」一声（不等识别，体感秒回）", d.id)
	}

	if err := dlg.WakeSilent(ctx, kw); err != nil {
		log.Printf("[%s] 唤醒失败: %v", d.id, err)
	}

	// 应答语**立刻**说，不等识别结果。
	//
	// 之前是等 ASR 出文本、确认"只说了唤醒词"之后才念，实测要 2~3 秒
	//（静音判定 600ms + ASR 500ms + TTS 起播），用户体感就是"喊了没反应"。
	// 现在直接播，代价是若用户一口气说完（"你好小黑今天天气"），应答会和
	// 提问重叠——所以这一句走 NoMute（不闭麦），并把回显的语气词剥掉。
	go func() {
		if said := dlg.SayWakeReply(ctx); said != "" {
			log.Printf("[%s] 唤醒应答（立即）: %q", d.id, said)
		}
	}()
}

// hasContent 判断识别结果里有没有实际内容。
//
// 安静时 VAD 也会偶尔给出一段"语音"，ASR 把它转成 "." 或 "嗯"。
// 免唤醒模式下这种碎片会不停触发对话，必须在进对话前挡掉。
func hasContent(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func (d *deviceAudio) handle(ctx context.Context, text string) {
	// 先落盘原始录音（后台打开"保存录音"时才做），失败不影响对话
	if d.opt.AudioDir != "" && len(d.lastPCM) > 0 {
		if path, err := audio.SaveWAV(d.opt.AudioDir, d.lastPCM); err != nil {
			log.Printf("[%s] 保存录音失败: %v", d.id, err)
		} else {
			log.Printf("[%s] 录音已保存 %s → %q", d.id, path, text)
		}
	}

	// 纯标点/空白的识别结果直接丢，别拿去问模型
	if !hasContent(text) {
		log.Printf("[%s] 识别结果没有实际内容，忽略: %q", d.id, text)
		if d.pipe != nil {
			d.pipe.Reset()
		}
		return
	}

	sess, err := d.hub.Get(d.id)
	if err != nil {
		return
	}
	dlg := d.dm.Session(d.id, &deviceTarget{hub: d.hub, id: d.id, sess: sess})

	// 免唤醒模式（仅调试）：没有 KWS 模型时，靠它验证整条链路。
	// 代价是家里任何对话都会被回答，所以默认关闭、界面上标了警告。
	if !dlg.Active() && d.freeTalk != nil && d.freeTalk() {
		// 静默唤醒：不念"我在"，否则每次说话都要多听一句
		if err := dlg.WakeSilent(ctx, "免唤醒"); err != nil {
			log.Printf("[%s] 免唤醒失败: %v", d.id, err)
			return
		}
	}

	d.mu.Lock()
	wa := d.wakeAt
	d.mu.Unlock()
	if !wa.IsZero() {
		asr := time.Duration(0)
		if !d.lastFrameAt.IsZero() {
			asr = time.Since(d.lastFrameAt) // 最后一帧到出文本 = ASR 推理
		}
		log.Printf("[%s] 识别出文本（距唤醒 %.2fs，其中 ASR 推理 %.2fs）: %q",
			d.id, time.Since(wa).Seconds(), asr.Seconds(), text)
	}

	// 唤醒应答是"不闭麦"播的，麦克风会把它自己收进来（"诶。"）。
	// 这类开头的语气词对后续理解没有意义，直接剥掉；
	// 剥完没内容就当作"只是应了一声"，不送给模型。
	if t := trimLeadingFillers(text); t != text {
		log.Printf("[%s] 剥掉开头语气词: %q → %q", d.id, text, t)
		text = t
	}
	if strings.TrimSpace(text) == "" {
		return
	}

	// 退出词先说：无论现在是不是在对话里都要应答。
	//
	// 之前只在"对话中"处理，导致用户说"退下"时如果对话已经静音超时，
	// 系统一声不吭——用户根本不知道到底退没退。
	// 这类"状态切换"的指令必须永远有反馈。
	if isExitCommand(text, dlg.ExitWords()) {
		wasActive := dlg.Active()
		dlg.Idle()
		log.Printf("[%s] 退出词命中: %q（之前%s）", d.id, text,
			map[bool]string{true: "在对话中", false: "已空闲"}[wasActive])
		t0 := time.Now()
		said := dlg.SayExitReply(ctx)
		ms := time.Since(t0).Milliseconds()
		d.dm.ReportEvent(&dialog.Turn{
			Device: d.id, ProfileID: dlg.ProfileID(), Text: text, Reply: said,
			TotalMS: ms, FirstAudioMS: ms,
		})
		if d.pipe != nil {
			d.pipe.Reset()
		}
		return
	}

	// 没被唤醒时的两条路：
	//   1) 文本唤醒（默认）：识别结果里出现唤醒词就进入对话，等价于"喊一声 → 应答"
	//   2) 都不匹配：忽略（家里聊天、电视声音不该触发回答）
	if !dlg.Active() {
		if d.wakeByASR != nil && d.wakeByASR() {
			if kw, rest, ok := agent.MatchWakeEx(text, d.wakeWords()); ok {
				log.Printf("[%s] 文本唤醒命中「%s」← %q（剩余提问=%q）", d.id, kw, text, rest)
				if rest == "" {
					// 只喊了唤醒词：应答"我在"，等下一句
					if err := dlg.Wake(ctx, kw); err != nil {
						log.Printf("[%s] 唤醒失败: %v", d.id, err)
					}
					if d.pipe != nil {
						d.pipe.Reset()
					}
					return
				}
				// 一句话里既喊了唤醒词又提了问题：静默唤醒后直接回答
				if err := dlg.WakeSilent(ctx, kw); err != nil {
					log.Printf("[%s] 唤醒失败: %v", d.id, err)
					return
				}
				text = rest
			}
		}
		log.Printf("[%s] 未唤醒，忽略识别结果: %q", d.id, text)
		if d.pipe != nil {
			d.pipe.Reset()
		}
		return
	}

	// KWS 已经在音频域唤醒过了，但 ASR 的文本里仍然带着那句唤醒词
	//（"你好小黑，今天天气怎么样"）。这里剥掉，否则会把唤醒词当提问的一部分。
	wakeWord := ""
	if kw, rest, ok := agent.MatchWakeEx(text, d.wakeWords()); ok {
		wakeWord = kw
		// 唤醒应答是"不闭麦"播的，ASR 可能把回声粘在唤醒词后面
		//（"你好，小黑哎。"），这里的"哎"不是提问。
		if t := trimLeadingFillers(rest); t != rest {
			log.Printf("[%s] 唤醒词后面只剩语气词: %q → 当作没说话", d.id, rest)
			rest = t
		}

		if rest == "" {
			// 只说了唤醒词：应答语在 KWS 命中那一刻已经播过了，这里不再重复。
			log.Printf("[%s] 只有唤醒词「%s」，等下一句", d.id, kw)
			if d.pipe != nil {
				d.pipe.Reset()
			}
			return
		}
		log.Printf("[%s] 剥掉唤醒词「%s」→ %q", d.id, kw, rest)
		text = rest
	}

	// 模型要 1.5~2.5 秒才吐第一个字，这段时间原来是完全静默的。
	// 先"叮"一声告诉用户"听到了，正在想"，感知延迟会小很多。
	//
	// 但刚在唤醒瞬间叮过的（<4 秒），这里就不要再叮：
	// 两声"叮"中间夹着两秒静默，听起来像卡了两次。

	// 回声过滤要放在最前面：闭麦尾巴盖不住的混响会被识别成
	// "用户复述了我们刚说的最后几个字"。这之后如果照常走流程，
	// 用户会听到一声"叮"然后没有任何回答——所以要在这之前拦下。
	if dlg.IsEcho(text) {
		log.Printf("[%s] 疑似听到自己的回声，忽略: %q", d.id, text)
		if d.pipe != nil {
			d.pipe.Reset()
		}
		return
	}

	d.mu.Lock()
	recentAck := !d.ackAt.IsZero() && time.Since(d.ackAt) < 4*time.Second
	d.mu.Unlock()
	if !recentAck && d.ackTone != nil && d.ackTone() {
		d.playAckTone(ctx)
	}

	d.mu.Lock()
	d.busy = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.busy = false
		d.mu.Unlock()
		if d.pipe != nil {
			d.pipe.Reset()
		}
	}()

	// 先落一条"处理中"：用户说完就想在后台看到这句话，
	// 不该等到模型答完（大模型 + TTS 可能要好几秒）。
	d.dm.ReportEvent(&dialog.Turn{
		Device: d.id, ProfileID: dlg.ProfileID(), WakeWord: wakeWord,
		Text: text, Pending: true,
	})

	if _, err := dlg.HandleText(ctx, text); err != nil {
		log.Printf("[%s] 对话失败: %v", d.id, err)
	}
}

// fillerWords 是开头的语气词/应答词，通常来自设备自己播的"诶""嗯"。
var fillerWords = []string{"诶", "欸", "嗯", "呃", "哦", "喔", "唉", "哎", "啊"}

// trimLeadingFillers 去掉开头的语气词与标点。
func trimLeadingFillers(text string) string {
	t := strings.TrimSpace(text)
	for {
		trimmed := false
		for _, w := range fillerWords {
			if strings.HasPrefix(t, w) {
				t = strings.TrimSpace(strings.TrimPrefix(t, w))
				trimmed = true
			}
		}
		// 再加上紧跟的标点
		t = strings.TrimLeft(t, "，,。.！!？?、 ")
		if !trimmed {
			return t
		}
	}
}

// exitCommands 是结束当前对话的说法（对齐 open-xiaoai-bridge 的 exit_keywords）。
var exitCommands = []string{"停止", "退出", "退下", "下去吧", "别说了", "再见", "结束"}

// isExitCommand 只在"整句就是个退出词"时成立，
// 避免"跟他说明天见""把音乐停止播放"这类正常话被误判成退出。
//
// 除了精确匹配，还做一次拼音容错：实测「退下」会被识别成「对下」
// （tuì xià / duì xià 只差一个声母），精确匹配就永远命不中，
// 用户喊好几遍都没反应，那句话还会被当成提问发给模型。
func isExitCommand(text string, words []string) bool {
	if len(words) == 0 {
		words = exitCommands // 兜底：配置还没加载时用内置默认
	}
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 6 {
		return false
	}
	for _, c := range words {
		if t == c || t == c+"吧" || t == c+"了" {
			return true
		}
	}
	// 拼音近似：先去掉句尾语气词（"退下了""对下吧"），整句很短且只差一个音才认。
	core := strings.TrimRight(t, "吧了呀啊嘛哦啦")
	if core == "" {
		return false
	}
	tp := audio.PinyinOf(core)
	if tp == "" {
		return false
	}
	for _, c := range words {
		cp := audio.PinyinOf(c)
		if cp == "" {
			continue
		}
		if editDistanceRunes(tp, cp) <= 1 && absInt(len([]rune(tp))-len([]rune(cp))) <= 1 {
			return true
		}
	}
	return false
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// editDistanceRunes 是字符串版莱文斯坦距离（按字符比较）。
func editDistanceRunes(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3i(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3i(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// playAckTone 让音箱本地响一声（不求回包，失败就算了）。
//
// 用本地提示音而不是 TTS：合成要几百毫秒，就失去"立刻反馈"的意义了。
func (d *deviceAudio) playAckTone(ctx context.Context) {
	sess, err := d.hub.Get(d.id)
	if err != nil {
		return
	}
	go func() {
		tctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		// "收到"= 上行双音（1200 → 1600），"说完了"= 下行双音（880 → 660），"出错"=低音两声。
		//
		// 实测原来的 1200Hz/60ms/振幅5000 确实播出去了（录音里峰值是底噪的上千倍），
		// 但在有背景音的环境里几乎听不见——太短、太轻。这里拉长到 200ms 总量、
		// 振幅提到 12000，并且用双音，明显得多。
		if _, err := sess.Request(tctx, proto.CmdPlayTone, proto.TonePayload{
			FreqHz: 1200, DurationMS: 90,
			Freq2Hz: 1600, Duration2MS: 110,
			Amplitude: 12000,
		}, 400*time.Millisecond); err != nil {
			// 提示音只是锦上添花，失败不打断对话，但要留痕
			log.Printf("[%s] 应答提示音失败: %v", d.id, err)
		}
	}()
}

// SayText 让音箱用自己的 TTS 念一段文字（走设备的云端合成）。
//
// 入队即返回：设备那边有自己的播放队列，服务端不必等它念完，
// 否则逐句下发会被串行化，一个三句的回答要多等好几秒。
func (t *deviceTarget) SayText(ctx context.Context, text string) error {
	sess := t.sess
	if sess == nil {
		var err error
		sess, err = t.hub.Get(t.id)
		if err != nil {
			return err
		}
	}
	_, err := sess.Request(ctx, proto.CmdSay, proto.SayPayload{Text: text}, 3*time.Second)
	return err
}

// StopSay 立刻停止设备当前播报并清空队列。
func (t *deviceTarget) StopSay(ctx context.Context) error {
	sess := t.sess
	if sess == nil {
		var err error
		sess, err = t.hub.Get(t.id)
		if err != nil {
			return err
		}
	}
	_, err := sess.Request(ctx, proto.CmdSayStop, nil, 2*time.Second)
	return err
}

// SayTextEx 播一段文字，可分别指定"不要收尾音""不要闭麦"。
func (t *deviceTarget) SayTextEx(ctx context.Context, text string, noDone, noMute bool) error {
	sess := t.sess
	if sess == nil {
		var err error
		sess, err = t.hub.Get(t.id)
		if err != nil {
			return err
		}
	}
	_, err := sess.Request(ctx, proto.CmdSay, proto.SayPayload{
		Text: text, NoDone: noDone, NoMute: noMute,
	}, 3*time.Second)
	return err
}

// SayTextQuiet 播一段文字，但播完不要"说完了"提示音（用于退出应答这类收尾语）。
func (t *deviceTarget) SayTextQuiet(ctx context.Context, text string) error {
	sess := t.sess
	if sess == nil {
		var err error
		sess, err = t.hub.Get(t.id)
		if err != nil {
			return err
		}
	}
	_, err := sess.Request(ctx, proto.CmdSay, proto.SayPayload{Text: text, NoDone: true}, 3*time.Second)
	return err
}

// deviceTarget 把 PCM 推给音箱播放。
type deviceTarget struct {
	hub  *hub.Hub
	id   string
	sess *hub.Session

	// 播放会话状态。实测：每句都重开一次 aplay，句间会多出 200~400ms 空档，
	// 一整段回答听起来就是"一句一停、很慢"。同一轮内连续下发时就复用同一个会话。
	mu         sync.Mutex
	started    bool
	lastRate   int
	playEndsAt time.Time // 已下发音频预计播完的时刻（含排队中的部分）
}

// playEndGrace 是"预计播完之后"再留的余量，覆盖 aplay 缓冲与网络抖动。
// playPrebuffer 与下发给设备的 PrebufferMS 保持一致。
const (
	playEndGrace  = 150 * time.Millisecond
	playPrebuffer = 120 * time.Millisecond
)

// PlayPCM 启动播放会话并分片下发。
// 分片 60ms：太大会导致打断迟滞，太小会增加帧开销。
func (t *deviceTarget) PlayPCM(ctx context.Context, pcm []byte, rate int) error {
	sess := t.sess
	if sess == nil {
		var err error
		sess, err = t.hub.Get(t.id)
		if err != nil {
			return err
		}
	}
	if rate <= 0 {
		rate = 24000
	}

	// 整段串行：既保证帧顺序，也让 playEndsAt 只有一个写入者。
	t.mu.Lock()
	defer t.mu.Unlock()

	// 不能用"距上次下发超过 700ms"来判断上一段播完了：
	// 下发只是把字节推进 socket，aplay 还在异步播；合成下一句要 1~2 秒，
	// 这段"发送侧空闲"里上一段往往还有大几百毫秒没播。那时重开会话会
	// 连旧 aplay 的缓冲一起杀掉，听感就是"半句突然断掉、直接接后文"。
	// 改用"已下发音频预计播完的时刻"判定，才和实际播放进度一致。
	needStart := t.shouldStart(rate, time.Now())
	if needStart {
		if _, err := sess.Request(ctx, proto.CmdPlayStart, proto.PlayStartPayload{
			SampleRate: rate, Channels: 1, PrebufferMS: int(playPrebuffer / time.Millisecond),
		}, 3e9); err != nil {
			return err
		}
		t.started = true
		t.lastRate = rate
		t.playEndsAt = time.Now().Add(playPrebuffer)
	}

	const chunkMS = 60
	bytesPerChunk := rate * chunkMS / 1000 * 2
	if bytesPerChunk < 2 {
		bytesPerChunk = 2
	}
	for off := 0; off < len(pcm); off += bytesPerChunk {
		end := off + bytesPerChunk
		if end > len(pcm) {
			end = len(pcm)
		}
		f := proto.NewAudioFrame(proto.FrameAudioDown, sess.NextAudioSeq(), uint16(rate), pcm[off:end])
		if err := sess.SendFrame(ctx, f); err != nil {
			return err
		}
	}

	// 新帧排在播放队列尾部：上一段没播完就接着它，播完了就从现在起算。
	t.notePCM(rate, len(pcm), time.Now())
	return nil
}

// shouldStart 判断此刻要不要重开播放会话。调用方持有 t.mu。
func (t *deviceTarget) shouldStart(rate int, now time.Time) bool {
	return !t.started || t.lastRate != rate || !now.Before(t.playEndsAt.Add(playEndGrace))
}

// notePCM 把本次下发的音频接在播放队列尾部，更新"预计播完时刻"。调用方持有 t.mu。
func (t *deviceTarget) notePCM(rate, nbytes int, now time.Time) {
	base := now
	if t.playEndsAt.After(base) {
		base = t.playEndsAt
	}
	dur := time.Duration(float64(nbytes/2) / float64(rate) * float64(time.Second))
	t.playEndsAt = base.Add(dur)
}
