// 音箱端客户端：连接服务端、采集麦克风、播放 TTS、执行受限命令。
//
// 编译（Mac 上交叉编译到音箱）：
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath \
//	  -ldflags "-s -w" -o dist/xiaoai-client-armv7 ./cmd/client
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"xiaoai-go/internal/client/audio"
	"xiaoai-go/internal/client/wsclient"
	"xiaoai-go/internal/proto"
)

// ttsPrepMu 串行化设备侧云端合成：预合成与正式播放可能同时要调 ubus，
// 并发调用会抢 /tmp/tts 下的临时文件。串行后预合成正好发生在上一段播放期间。
var ttsPrepMu sync.Mutex

var (
	flagServer = flag.String("server", "ws://127.0.0.1:4399/ws", "服务端 WebSocket 地址（装到音箱上后由 /data/xiaoai/server.txt 覆盖）")
	flagID     = flag.String("id", "lx06", "设备 ID")
	flagToken  = flag.String("token", "", "设备鉴权 token")
	flagDevice = flag.String("device", "noop", "ALSA 采集设备")
	flagProbe  = flag.Bool("probe", false, "只探测服务端是否可达（供开机脚本用）")
	flagDebug  = flag.Bool("debug", false, "输出调试日志")
	flagWake   = flag.String("wakewords", defaultWakePath, "唤醒词清单落盘路径（由服务端下发，别指向 sherpa 的 keywords.txt）")
)

const clientVersion = "2.0.0"

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if !*flagDebug {
		log.SetOutput(os.Stdout)
	}

	if *flagProbe {
		os.Exit(probe())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 队列放宽到 32：服务端会边生成边下发多段，客户端要能先收着，
	// 否则非阻塞入队会把后到的段落丢掉（听起来就是缺字）。
	a := &agent{cfg: audio.DefaultCapture(), sayCh: make(chan sayItem, 32), silentCheck: &silentCapture{}}
	a.cfg.Device = *flagDevice

	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		if err := a.connectAndServe(ctx); err != nil {
			log.Printf("连接结束: %v", err)
		} else {
			log.Printf("连接已被对端正常关闭")
		}
		a.cleanup()

		// 指数退避 + 抖动，避免服务端重启时的重连风暴
		wait := jitter(backoff)
		log.Printf("%s 后重连（当前退避 %s）", wait.Round(time.Millisecond), backoff.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < 15*time.Second {
			backoff = time.Duration(float64(backoff) * 1.6)
		}
	}
}

func jitter(d time.Duration) time.Duration {
	f := 0.8 + rand.Float64()*0.4 // ±20%
	return time.Duration(float64(d) * f)
}

// agent 持有一次连接期间的所有状态。
// sayItem 是一条待播内容：文字 + 是否要在播完后省略"说完了"提示音。
type sayItem struct {
	text   string
	noDone bool
	noMute bool
}

type agent struct {
	cfg     audio.CaptureConfig
	ws      *wsclient.Client
	rec     *audio.Recorder
	ply     *audio.Player
	mu      sync.Mutex
	wantRec bool // 期望在录音（看门狗据此决定要不要重开麦）

	// 预计播完的时刻。用它判断"我还出不出声"，比 mphelper 可靠：
	// mphelper mute_stat 反映的是小爱自己的静音状态，我们 aplay 的声音它看不到。
	playUntil time.Time

	// 原生 TTS 的播放队列：服务端把句子丢过来，这边按顺序念，
	// 入队即回包，不要等播完（否则整个回答会被串行化，慢得没法用）。
	sayCh chan sayItem

	// saying 表示「设备正在用原生 TTS 出声」。
	// 这段时间必须闭麦：tts_play.sh 是阻塞的（miplayer -f 播完才返回），
	// 而麦克风一直在采，把自己念的话当成人声传上去，服务端就会当成新提问，
	// 于是自问自答、无限循环。用 atomic 是为了在音频回调里零成本读取。
	saying atomic.Bool

	// sayCancel 用于打断当前播报
	sayCancel context.CancelFunc
	// idleBeep 是"这一轮说完了"的待响提示音。
	// 放在结构体里而不是循环局部变量，是为了让"打断"能把它一起取消——
	// 否则播报被掐断后 400ms 还会冒出一声"说完了"。
	idleBeep *time.Timer

	// 采集静音检测（设备选错时的唯一线索）
	silentCheck *silentCapture

	rtt      time.Duration
	stopOnce sync.Once
}

func (a *agent) connectAndServe(parent context.Context) error {
	// 每条连接一个子 context，连接结束就取消。
	//
	// 这个不能省：之前心跳/播报/轮询/看门狗都挂在进程级 ctx 上，
	// 而 main 是"断了就重连"的循环——每重连一次就多起一套协程，
	// 且永不退出。攒到后面会有好几个 sayWorker 同时从同一个队列取句子，
	// 各跑一个 tts_play.sh，听起来就是"两个声音一起说话"。
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	dialCtx, cancelDial := context.WithTimeout(ctx, 6*time.Second)
	defer cancelDial()
	cli, err := wsclient.Dial(dialCtx, *flagServer, wsclient.DialOptions{Token: *flagToken})
	if err != nil {
		return err
	}
	a.ws = cli
	defer cli.Close("bye")

	// 进程收到 SIGTERM 时只是取消了 ctx，而 Recv 阻塞在 socket 读上不会自己醒，
	// 结果就是"杀不掉、只能 kill -9"。这里在 ctx 结束时主动关连接把它踢醒。
	go func() {
		<-ctx.Done()
		cli.Close("shutdown")
	}()

	// 握手
	hello := proto.NewEnvelope(proto.TypeHello, "", "c:1")
	raw, _ := json.Marshal(proto.HelloPayload{
		DeviceID: *flagID, Model: modelName(), Firmware: firmwareVersion(),
		Version: clientVersion, Protocol: proto.Version,
		Caps: []string{"record", "play", "tone", "shell", "info"},
	})
	hello.Data = raw
	if err := cli.Send(ctx, hello); err != nil {
		return fmt.Errorf("发送握手失败: %w", err)
	}
	log.Printf("已连接 %s，等待握手应答", *flagServer)

	// 心跳
	go a.heartbeat(ctx)

	// 原生 TTS 播放队列：先清掉上一条连接残留的句子，
	// 否则重连后会把上一轮没说完的话接着念出来
	drainSayQueue(a.sayCh)
	go a.sayWorker(ctx)

	// 播放状态轮询
	go a.pollPlaying(ctx)

	// 录音看门狗：音箱上的 arecord 会被系统服务抢走设备而静默退出，
	// 不重启就永久静音，而日志里只有一行"停止"，很难查。
	go a.watchRecording(ctx)

	for {
		env, frame, err := cli.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if wsclient.IsNormalClose(err) {
				log.Printf("对端关闭连接: %v", err)
				return nil
			}
			return err
		}
		if frame != nil {
			a.onFrame(frame)
			continue
		}
		a.onEnvelope(ctx, env)
	}
}

func (a *agent) heartbeat(ctx context.Context) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = a.ws.Ping(ctx)
		}
	}
}

func (a *agent) onEnvelope(ctx context.Context, e *proto.Envelope) {
	switch e.Type {
	case proto.TypeHelloAck:
		var ack proto.HelloAck
		_ = e.Decode(&ack)
		log.Printf("握手成功: 协议 v%d，心跳 %dms，上行帧长 %dms", ack.Protocol, ack.HeartbeatMS, ack.FrameMS)
		if ack.FrameMS > 0 {
			a.cfg.FrameMS = ack.FrameMS
		}
		// 连上就开麦，服务端随时能听到唤醒词
		a.startRecording(ctx)
	case proto.TypePong:
		a.mu.Lock()
		a.rtt = time.Duration(proto.NowMS()-e.TS) * time.Millisecond
		a.mu.Unlock()
	case proto.TypeReq:
		go a.handleRequest(ctx, e)
	case proto.TypeError:
		log.Printf("服务端报错: %d %s", e.Code, e.Msg)
	}
}

func (a *agent) handleRequest(ctx context.Context, req *proto.Envelope) {
	resp, err := a.dispatch(ctx, req)
	if err != nil {
		resp = proto.NewErrResp(req.ID, proto.CodeInternal, err.Error())
	}
	resp.ID = req.ID // 必须回填请求 id，否则对端要等超时
	if sendErr := a.ws.Send(ctx, resp); sendErr != nil {
		log.Printf("回包失败(%s): %v", req.Cmd, sendErr)
	}
}

func (a *agent) dispatch(ctx context.Context, req *proto.Envelope) (*proto.Envelope, error) {
	switch req.Cmd {
	case proto.CmdDeviceInfo:
		return proto.NewResp(req.ID, a.deviceInfo())

	case proto.CmdRecordStart:
		var p proto.RecordStartPayload
		if err := req.Decode(&p); err != nil {
			return nil, err
		}
		if p.SampleRate > 0 {
			a.cfg.SampleRate = p.SampleRate
		}
		if p.FrameMS > 0 {
			a.cfg.FrameMS = p.FrameMS
		}
		if p.Device != "" {
			a.cfg.Device = p.Device
		}
		a.startRecording(ctx)
		return proto.NewResp(req.ID, nil)

	case proto.CmdRecordStop:
		a.stopRecording()
		return proto.NewResp(req.ID, nil)

	case proto.CmdPlayStart:
		var p proto.PlayStartPayload
		_ = req.Decode(&p)
		// 兜底：服务端若因为会话状态重建又发了一次 play.start，而上一段 PCM
		// 还在播，重启播放器会把已下发但未播完的尾部一起丢掉。仍在播就复用现有会话。
		if a.pcmPlaying() && a.player().Running() {
			log.Printf("忽略重复的 play.start：上一段音频仍在播放")
			return proto.NewResp(req.ID, nil)
		}
		cfg := audio.PlayConfig{SampleRate: p.SampleRate, Channels: p.Channels, PrebufferMS: p.PrebufferMS}
		if cfg.SampleRate == 0 {
			cfg.SampleRate = 24000
		}
		if cfg.PrebufferMS == 0 {
			cfg.PrebufferMS = 120
		}
		a.player().Stop(true)
		a.mu.Lock()
		a.ply = audio.NewPlayer()
		ply := a.ply
		a.mu.Unlock()
		if err := ply.Start(cfg); err != nil {
			return nil, err
		}
		return proto.NewResp(req.ID, nil)

	case proto.CmdPlayStop:
		a.player().Stop(true)
		return proto.NewResp(req.ID, nil)

	case proto.CmdPlayTone:
		var p proto.TonePayload
		_ = req.Decode(&p)
		rate := 24000
		pcm := audio.TonePCM(rate, p.FreqHz, p.DurationMS, p.Gate, p.Amplitude)
		if p.Freq2Hz > 0 && p.Duration2MS > 0 {
			pcm = append(pcm, audio.TonePCM(rate, p.Freq2Hz, p.Duration2MS, p.Gate, p.Amplitude)...)
		}
		ply := a.player()
		_ = ply.Start(audio.PlayConfig{SampleRate: rate, Channels: 1, PrebufferMS: 0})
		if err := ply.Write(pcm); err != nil {
			return nil, err
		}
		return proto.NewResp(req.ID, nil)

	case proto.CmdSayStop:
		a.stopSaying()
		return proto.NewResp(req.ID, nil)

	case proto.CmdShellRun:
		var p proto.ShellPayload
		if err := req.Decode(&p); err != nil {
			return nil, err
		}
		res := runShell(ctx, p.Script, p.TimeoutMS)
		return proto.NewResp(req.ID, res)

	case proto.CmdSay:
		var p proto.SayPayload
		if err := req.Decode(&p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Text) == "" {
			return proto.NewResp(req.ID, proto.SayResult{Queued: false})
		}
		log.Printf("收到播报请求: %s", truncateStr(p.Text, 20))
		select {
		case a.sayCh <- sayItem{text: p.Text, noDone: p.NoDone, noMute: p.NoMute}:
			// 入队即开始预合成：服务端会把下一段在上一段还在播的时候发过来，
			// 这段云端往返正好和播放重叠。等轮到它再合成就会空出 1~2 秒。
			go a.warmTTS(ctx, p.Text)
			return proto.NewResp(req.ID, proto.SayResult{Queued: true})
		default:
			// 队列满说明前面还堆着好几段，丢掉新的比堆积更好
			log.Printf("原生 TTS 队列已满，丢弃: %q", truncateStr(p.Text, 20))
			return proto.NewResp(req.ID, proto.SayResult{Queued: false})
		}

	case proto.CmdKWSSync:
		var p proto.KWSSyncPayload
		if err := req.Decode(&p); err != nil {
			return nil, err
		}
		res, err := writeKeywords(p, *flagWake)
		if err != nil {
			return proto.NewErrResp(req.ID, proto.CodeInternal, "同步唤醒词失败: "+err.Error()), nil
		}
		log.Printf("唤醒词已同步: %d 个 → %s (rev=%s%s)", res.Count, res.Path, res.Rev,
			map[bool]string{true: ", 未改动", false: ""}[res.Skip])
		return proto.NewResp(req.ID, res)

	case proto.CmdVolumeSet:
		// 音量接口因机型而异，P2 在真机上探测后再实现，这里明确返回不支持，
		// 避免上层误以为设置成功。
		return proto.NewErrResp(req.ID, proto.CodeUnsupported, "音量控制尚未实现"), nil
	}
	return proto.NewErrResp(req.ID, proto.CodeUnknownCmd, "未知命令: "+req.Cmd), nil
}

// onFrame 处理下行音频（服务端 TTS）。
func (a *agent) onFrame(f *proto.Frame) {
	if f.Type != proto.FrameAudioDown {
		return
	}
	if f.Flags.Has(proto.FlagInterrupt) {
		a.player().Stop(true)
		a.mu.Lock()
		a.playUntil = time.Time{}
		a.mu.Unlock()
	}
	if err := a.player().Write(f.Payload); err != nil {
		log.Printf("写入播放器失败: %v", err)
	}

	// 累加"还要播多久"：每帧 PCM 的长度除以采样率就是这帧的时长
	rate := int(f.SampleRate)
	if rate <= 0 {
		rate = 24000
	}
	dur := time.Duration(float64(len(f.Payload)/2) / float64(rate) * float64(time.Second))

	a.mu.Lock()
	base := a.playUntil
	if base.Before(time.Now()) {
		base = time.Now()
	}
	a.playUntil = base.Add(dur)
	a.mu.Unlock()
}

// pcmPlaying 只看本地 PCM 链路是不是还在出声（留 250ms 余量给 aplay 缓冲）。
// 与 stillPlaying 分开：设备原生 TTS 期间不该阻止 play.start。
func (a *agent) pcmPlaying() bool {
	a.mu.Lock()
	until := a.playUntil
	a.mu.Unlock()
	return time.Now().Before(until.Add(250 * time.Millisecond))
}

// stillPlaying 判断自己是否还在出声。
func (a *agent) stillPlaying() bool {
	// 原生 TTS 期间也算在播：让服务端同步闭麦，别把音箱的话当提问
	return a.saying.Load() || a.pcmPlaying()
}

// sayWorker 按顺序把文字交给设备原生 TTS 念出来。
//
// 走 /usr/sbin/tts_play.sh → ubus call mibrain text_to_speech → 系统播放器。
// 音色由设备固件决定（实测和"小爱同学"平时的应答音色并不完全一样，
// 这条通道拿到的是 mibrain 合成的 mp3）。调用会阻塞到播放结束，所以必须串行。
func (a *agent) sayWorker(ctx context.Context) {
	// 队列空着再等一小会儿，期间没有新内容进来才认为整段说完，补一声提示音。
	// 这样提示音不会盖住最后一句（设备原生 TTS 一轮常有多条）。
	for {
		a.mu.Lock()
		idle := a.idleBeep
		a.mu.Unlock()

		var idleC <-chan time.Time
		if idle != nil {
			idleC = idle.C
		}
		select {
		case <-ctx.Done():
			a.stopIdleBeep()
			return
		case <-idleC:
			a.stopIdleBeep()
			log.Print("提示音: 说完")
			a.beep(audio.BeepDone)
		case item := <-a.sayCh:
			a.stopIdleBeep()
			a.say(ctx, item.text, item.noMute)
			if item.noDone {
				// 收尾语：播完就完了，不补提示音
				continue
			}
			a.mu.Lock()
			a.idleBeep = time.NewTimer(400 * time.Millisecond)
			a.mu.Unlock()
		}
	}
}

// stopIdleBeep 取消待响的"说完了"提示音（被打断时不该再响）。
func (a *agent) stopIdleBeep() {
	a.mu.Lock()
	if a.idleBeep != nil {
		a.idleBeep.Stop()
		a.idleBeep = nil
	}
	a.mu.Unlock()
}

// stopSaying 立刻停止当前播报并清空队列。
func (a *agent) stopSaying() {
	a.mu.Lock()
	cancel := a.sayCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	drainSayQueue(a.sayCh)
	a.stopIdleBeep()
	// 播放器不用在这里杀：say() 用的是 runShellStoppable，
	// 上面的 cancel 会让它把整个进程组（sh + miplayer）一起终止。
	// 原来这里 pkill -x miplayer 会把用户正在放的音乐也掐掉。
}

// say 念一句（阻塞到播完），期间闭麦。
func (a *agent) say(ctx context.Context, text string, noMute bool) {
	// 单独一个可取消的 ctx：stopSaying() 靠它打断正在念的内容
	sayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.sayCancel = cancel
	a.mu.Unlock()

	t0 := time.Now()
	// 先取音频：命中最快（缓存），否则走一次云端合成。
	// 合成期间不闭麦——这段时间没有任何声音，用户随时可能开口。
	path, cached, err := a.prepareTTS(sayCtx, text)
	if err != nil {
		log.Printf("原生 TTS 合成失败，退回脚本路径: %v", err)
		a.sayLegacy(sayCtx, ctx, text, noMute)
		return
	}
	prepMS := time.Since(t0).Milliseconds()

	// noMute：唤醒应答这种一两声的短句不闭麦，避免吞掉用户紧接着说的话
	if !noMute {
		a.saying.Store(true)
	}
	if out := runShellStoppable(sayCtx, playTTSScript(path), 60000); out.ExitCode != 0 && !ttsPlayOK(out) {
		log.Printf("播放失败(code=%d): %s", out.ExitCode,
			truncateStr(strings.TrimSpace(out.Stderr+out.Stdout), 120))
	}
	// 播完只留 80ms 收尾。原来留 250ms，等于每句之后固定吞掉 0.25 秒——
	// 用户紧接着说话时，开头就被这段"保险"吃掉了。
	select {
	case <-ctx.Done():
	case <-time.After(80 * time.Millisecond):
	}
	a.saying.Store(false)

	tag := "合成"
	if cached {
		tag = "缓存"
	}
	log.Printf("念完(%.1fs, %s %dms): %s", time.Since(t0).Seconds(), tag, prepMS, truncateStr(text, 24))
}

// warmTTS 在文本入队时就提前把音频备好，别等轮到它播才去 ubus。
// 服务端会在上一段还在播的时候把下一段发过来，这两段时间正好重叠；
// 即使真分成多段，段间也不会再空出一次云端合成的往返。
func (a *agent) warmTTS(ctx context.Context, text string) {
	if _, ok := cachedTTS(text); ok {
		return
	}
	if _, _, err := a.prepareTTS(ctx, text); err != nil {
		log.Printf("预合成失败(%s): %v", truncateStr(text, 16), err)
	}
}

// prepareTTS 拿到可播放的本地文件：缓存命中直接返回，否则调 ubus 合成并存入缓存。
func (a *agent) prepareTTS(ctx context.Context, text string) (path string, cached bool, err error) {
	ttsPrepMu.Lock()
	defer ttsPrepMu.Unlock()
	if p, ok := cachedTTS(text); ok {
		return p, true, nil
	}
	out := runShell(ctx, "ubus call mibrain text_to_speech "+shQuote(ttsJSON(text)), 20000)
	raw := out.Stdout + out.Stderr
	path = parseTTSPath(raw)
	// 路径要真的是 /tmp 下一个像样的音频文件才用：这段字符串来自设备侧的
	// 另一个进程，格式一旦变了，原来的实现会拿着它直接去 miplayer，
	// 表现成"卡几秒然后播放失败"，日志里什么都看不出来。
	if path != "" && !validateTTSPath(path) {
		log.Printf("ubus 返回的音频路径不可用(%s)，原始输出: %s", path, truncateStr(raw, 200))
		path = ""
	}
	if path == "" {
		return "", false, fmt.Errorf("ubus 没返回音频路径: %s", truncateStr(strings.TrimSpace(raw), 100))
	}
	return saveTTS(path, text), false, nil
}

// sayLegacy 是旧路径：tts_play.sh 一次做完"合成 + 播放"。
// 只在自建合成链路失败时兜底（不同固件的 ubus 返回格式可能不一样）。
func (a *agent) sayLegacy(sayCtx, ctx context.Context, text string, noMute bool) {
	if !noMute {
		a.saying.Store(true)
	}
	out := runShellStoppable(sayCtx, ttsScript(text), 30000)
	select {
	case <-ctx.Done():
	case <-time.After(80 * time.Millisecond):
	}
	a.saying.Store(false)
	if !ttsPlayOK(out) {
		log.Printf("原生 TTS 失败(code=%d): %s", out.ExitCode,
			truncateStr(strings.TrimSpace(out.Stderr+out.Stdout), 120))
	}
}

// ttsJSON 生成 ubus 需要的参数。用 json.Marshal 保证引号/换行被正确转义。
func ttsJSON(text string) string {
	b, _ := json.Marshal(map[string]any{"text": text, "save": 1})
	return string(b)
}

// playTTSScript 播一个已经存在的音频文件。
// 先 mphelper pause：小爱可能正在放歌/播报，不暂停会和我们的声音叠在一起。
func playTTSScript(path string) string {
	return "/usr/bin/mphelper pause; miplayer -f " + shQuote(path)
}

// beep 本地响一声（不走云端 TTS，几十毫秒内出声）。
func (a *agent) beep(gen func(int) []byte) {
	const rate = 24000
	ply := a.player()
	if ply == nil {
		return
	}
	if err := ply.Start(audio.PlayConfig{SampleRate: rate, Channels: 1, PrebufferMS: 0}); err != nil {
		return
	}
	_ = ply.Write(gen(rate))
}

// ttsScript 包装设备的 TTS 脚本。/usr/sbin/tts_play.sh 有两处坑：
//   - 它调用 my_log，但函数定义在 /usr/bin/voip_helper 里，直接跑会报 "my_log: not found"
//   - 它内部用 "$1" 取文本，所以要用 set -- 把文本变成位置参数再 source
func ttsScript(text string) string {
	return "my_log() { :; }; set -- " + shQuote(text) + `; . /usr/sbin/tts_play.sh "$@"`
}

// ttsPlayOK 判断原生 TTS 是否真的播了。
// 不能看退出码：tts_play.sh 末尾是 `[ mute_stat == 1 ] && { mphelper play }`，
// 在"原本没在播"的情况下这个条件天然为假、脚本返回 1，而话其实已经播完了。
// 真正的判据是：ubus 给出了音频路径，或者播放器走到了 EndReached。
func ttsPlayOK(out proto.ShellResult) bool {
	s := out.Stdout + out.Stderr
	return strings.Contains(s, "/tmp/tts/") || strings.Contains(s, "EndReached")
}

// drainSayQueue 清空待播队列（重连后不该接着念旧内容）。
func drainSayQueue(ch chan sayItem) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// shQuote 用单引号包一层 shell 参数，内部的单引号按 POSIX 惯例转义。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (a *agent) player() *audio.Player {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ply == nil {
		a.ply = audio.NewPlayer()
	}
	return a.ply
}

func (a *agent) watchRecording(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var lastTry time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			want := a.wantRec
			alive := a.rec != nil && a.rec.Running()
			a.mu.Unlock()
			if !want || alive {
				continue
			}
			// 重启也别太急：设备被占用时频繁重试会刷屏
			if time.Since(lastTry) < 2*time.Second {
				continue
			}
			lastTry = time.Now()
			log.Print("录音已中断（arecord 可能被系统抢占），重新开麦")
			a.startRecording(ctx)
		}
	}
}

// silentCapture 用来发现「采集设备选错了，但一切看起来都正常」这种情况：
// arecord 正常跑、字节在流动、日志零报错，只是数据全是 0。
// 这台 LX06 上的 hw:0,1 / hw:0,2 就是这种哑巴通道（能开、有字节、恒静音）。
type silentCapture struct {
	lastSound atomic.Int64 // 最近一次有声音的时间（unix nano）
	warned    atomic.Bool
}

func checkSilentCapture(s *silentCapture, lv audio.Level) {
	now := time.Now()
	if lv.Peak > 0 {
		s.lastSound.Store(now.UnixNano())
		return
	}
	last := s.lastSound.Load()
	if last == 0 {
		s.lastSound.Store(now.UnixNano())
		return
	}
	if !s.warned.Load() && now.Sub(time.Unix(0, last)) > 10*time.Second {
		s.warned.Store(true)
		log.Print("警告: 采集已连续 10 秒全是静音，采集设备可能选错了 —— " +
			"这台 LX06 要用 noop（厂商给 PDM 麦克风起的别名），hw:0,1/hw:0,2 是恒静音的 dummy 通道")
	}
}

func (a *agent) startRecording(ctx context.Context) {
	a.mu.Lock()
	a.wantRec = true
	if a.rec != nil && a.rec.Running() {
		a.mu.Unlock()
		return
	}
	rec := audio.NewRecorder(a.cfg)
	a.rec = rec
	cfg := a.cfg
	a.mu.Unlock()

	err := rec.Start(ctx,
		func(payload []byte) {
			// 设备自己在出声时不上行：这段音频必然是它自己念的话被收回来的，
			// 传上去只会被 ASR 认成新的提问（"自己跟自己聊"）。
			if a.saying.Load() {
				return
			}
			f := proto.NewAudioFrame(proto.FrameAudioUp, a.ws.NextSeq(), uint16(cfg.SampleRate), payload)
			f.TimestampUS = time.Now().UnixMicro()
			if err := a.ws.SendFrame(ctx, f); err != nil {
				log.Printf("上送音频失败: %v", err)
			}
		},
		func(lv audio.Level) {
			checkSilentCapture(a.silentCheck, lv)
			ev, err := proto.NewEvent(proto.EvAudio, proto.AudioLevel{
				Level: lv.Level, RMS: lv.RMS, Peak: lv.Peak,
			})
			if err == nil {
				_ = a.ws.Send(ctx, ev)
			}
		},
	)
	if err != nil {
		log.Printf("启动录音失败: %v", err)
		return
	}
	log.Printf("开始录音: %s %dHz %dms", cfg.Device, cfg.SampleRate, cfg.FrameMS)
}

func (a *agent) stopRecording() {
	a.mu.Lock()
	a.wantRec = false
	rec := a.rec
	a.rec = nil
	a.mu.Unlock()
	if rec != nil {
		rec.Stop()
		log.Print("停止录音")
	}
}

// pollPlaying 轮询小爱的播放状态（Rust 版也是 10ms 轮询 mute_stat，这里放宽到 300ms）。
func (a *agent) pollPlaying(ctx context.Context) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := "idle"
			// 只信"我们自己在出声"这一个信号。
			//
			// 曾经用 mphelper mute_stat 兼容小爱原生播放，但实测这个命令
			// 在空闲和播放时**都返回 2**，而 2 被映射成 paused、paused 又被
			// 服务端当成"正在播"处理 —— 结果播放状态永远停在 paused，
			// 服务端一路闭麦，喊唤醒词完全没反应（踩过一次，很难查）。
			// 只有明确返回 1 时才当作在播；其它值一律忽略。
			if a.stillPlaying() {
				s = "playing"
			} else if out, err := exec.CommandContext(ctx, "mphelper", "mute_stat").Output(); err == nil {
				if strings.TrimSpace(string(out)) == "1" {
					s = "playing"
				}
			}
			if s != last {
				last = s
				ev, err := proto.NewEvent(proto.EvPlaying, proto.PlayingState{State: s})
				if err == nil {
					_ = a.ws.Send(ctx, ev)
				}
			}
		}
	}
}

func (a *agent) deviceInfo() proto.DeviceInfo {
	a.mu.Lock()
	rtt := a.rtt
	a.mu.Unlock()

	info := proto.DeviceInfo{
		Model:      modelName(),
		Firmware:   firmwareVersion(),
		UptimeSec:  uptimeSec(),
		MemFreeMB:  memFreeMB(),
		Load1:      load1(),
		DiskFreeMB: diskFreeMB("/data"),
	}
	_ = rtt
	return info
}

func (a *agent) cleanup() {
	a.stopRecording()
	a.mu.Lock()
	ply := a.ply
	a.ply = nil
	a.mu.Unlock()
	if ply != nil {
		ply.Stop(true)
	}
}

// runShell 执行脚本并收集输出。
func runShell(ctx context.Context, script string, timeoutMS int) proto.ShellResult {
	if timeoutMS <= 0 {
		timeoutMS = 10000
	}
	c, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(c, "/bin/sh", "-c", script)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return shellResult(stdout.String(), stderr.String(), err)
}

// shellResult 把 exec 的结果整理成协议结构。
func shellResult(stdout, stderr string, err error) proto.ShellResult {
	res := proto.ShellResult{Stdout: stdout, Stderr: stderr}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			if res.Stderr == "" {
				res.Stderr = err.Error()
			}
		}
	}
	return res
}

// runShellStoppable 和 runShell 一样，但取消时终止**整个进程组**。
//
// 只给"会拉起子播放器"的脚本用：`sh -c 'mphelper pause; miplayer -f x'`
// 里 miplayer 是 sh 的子进程，CommandContext 只杀 sh，播放会继续——
// 之前就是靠 pkill 兜底，结果连用户正在放的音乐一起掐掉。
func runShellStoppable(ctx context.Context, script string, timeoutMS int) proto.ShellResult {
	if timeoutMS <= 0 {
		timeoutMS = 10000
	}
	c, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", script)
	startNewProcessGroup(cmd)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return proto.ShellResult{ExitCode: -1, Stderr: err.Error()}
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		return shellResult(stdout.String(), stderr.String(), err)
	case <-c.Done():
		killProcessGroup(cmd)
		<-waitDone
		res := shellResult(stdout.String(), stderr.String(), c.Err())
		res.ExitCode = -1
		return res
	}
}

// probe 仅探测服务端可达性，供开机脚本等待网络用。
func probe() int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := wsclient.Dial(ctx, *flagServer, wsclient.DialOptions{Token: *flagToken, Timeout: 3 * time.Second})
	if err != nil {
		return 1
	}
	cli.Close("probe")
	return 0
}

// ---- 设备信息读取（linux 优先，其它平台降级为 0） ----

func uptimeSec() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func memFreeMB() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

func load1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
