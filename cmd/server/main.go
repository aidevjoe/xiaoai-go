// 服务端：设备面（WebSocket）+ 管理面（HTTP）。
//
// 当前阶段（P1）只做链路：握手、录音上行、TTS 下行、提示音、shell、健康检查。
// 音频识别与对话状态机在 P2/P3 接入 internal/server/audio 与 dialog。
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"xiaoai-go/internal/proto"
	"xiaoai-go/internal/server/agent"
	"xiaoai-go/internal/server/audio"
	"xiaoai-go/internal/server/dialog"
	"xiaoai-go/internal/server/hub"
	"xiaoai-go/internal/server/store"
	"xiaoai-go/internal/server/tools"
	"xiaoai-go/internal/server/web"
)

const serverVersion = "2.0.0-dev"

var (
	flagDeviceAddr = flag.String("device-addr", ":4399", "设备面监听地址（WebSocket）")
	flagAdminAddr  = flag.String("admin-addr", ":8080", "管理面监听地址（HTTP）")
	flagToken      = flag.String("token", "", "设备鉴权 token，空则不校验")

	flagModels      = flag.String("models", "", "模型根目录（含 silero_vad.onnx / sense-voice / vits）")
	flagASRDir      = flag.String("asr-dir", "", "SenseVoice 模型目录，留空则按 models 约定推导")
	flagTTSModel    = flag.String("tts-model", "", "VITS 模型 .onnx 路径，留空则不加载 TTS")
	flagNoEngine    = flag.Bool("no-engine", false, "跳过模型加载（只测链路）")
	flagEngineDebug = flag.Int("engine-debug", 0, "1 = 打印模型加载细节")

	flagProfiles = flag.String("profiles", "", "助手档案 JSON 文件（多唤醒词 → 多 LLM，只读导入）")
	flagSettings = flag.String("settings", "settings.json", "后台可改配置的存放路径（会自动创建）")
	flagHAURL    = flag.String("ha-url", "", "Home Assistant 地址，如 http://homeassistant.local:8123")
	flagHAToken  = flag.String("ha-token", "", "HA 长期访问令牌")

	// 没有 -profiles 时用这几个参数生成一个默认档案
	flagLLMBase   = flag.String("llm-base", "", "默认档案的模型地址")
	flagLLMKey    = flag.String("llm-key", "", "默认档案的模型密钥")
	flagLLMModel  = flag.String("llm-model", "", "默认档案的模型名")
	flagWakeWords = flag.String("wake-words", "小黑,你好小黑", "默认档案的唤醒词，逗号分隔")
)

type server struct {
	h *hub.Hub

	// 引擎与工具都支持热替换（后台改配置 / 点重载），统一用锁保护
	engMu     sync.RWMutex
	engine    *engineHandle // 带引用计数，见 engine.go
	dmEngine  *engineHandle // 对话框当前替谁持着引用（可能是已退休的旧引擎）
	engineErr error

	// reg 用原子指针：applySettings 在线程 A 换档案，设备协程在线程 B 读。
	// 普通指针赋值虽然不会崩，但读到的是"新指针 + 旧内部状态"的混合快照，
	// race detector 也会直接报 DATA RACE。
	reg     atomic.Pointer[agent.Registry]
	store   *store.Manager
	cfg     store.Settings // 当前配置快照（deviceAudio 建管道时读）
	cfgMu   sync.RWMutex
	turns   *turnLog
	tools   *tools.Registry
	toolsMu sync.RWMutex
	// speech 是 TTS 路由层：按"档案 → 供应商 → 音色"决定这一句用谁合成。
	// 全局供应商的实例就装在引擎里，Service 直接借用，不重复加载。
	speech  *audio.Service
	haURL   string // 当前工具集用的 HA 地址（变了才重建客户端）
	haToken string
	webSig  string // 搜索配置指纹（变了才重建客户端，保留缓存）
	wxSig   string // 天气配置指纹
	// customSig / customNames 跟踪「配置化自定义工具」：配置变了先摘旧的再注册新的，
	// 否则改一个地址会留下一个同名旧工具。
	customSig   string
	customNames []string

	// 后台模块注册表（插件预留，见 modules.go）
	modulesMu sync.RWMutex
	modules   []adminModule
	dm        *dialog.Manager
	devAudio  map[string]*deviceAudio

	// 唤醒词同步状态（device id → 最近一次结果）
	kwsMu sync.Mutex
	kws   map[string]kwsState

	// 唤醒词一改就要重建 KWS（keywords.txt 是引擎构造时读进去的）。
	// 后台自动保存可能短时间内连发几次，串行化并合并：正在重载时再来的
	// 请求只留一个"再来一次"的标记，不会排队重载 N 遍。
	reloadMu    sync.Mutex
	reloadBusy  bool
	reloadAgain bool

	// 播放状态：播报期间闭麦，否则音箱会听见自己说话（回音环）
	playMu sync.Mutex
	play   map[string]playInfo

	devMu    sync.Mutex
	lastTurn *dialog.Turn

	mu       sync.Mutex
	sessions map[string]*deviceStat

	startedAt time.Time
	frames    atomic.Int64 // 收到的音频帧总数
	bytes     atomic.Int64 // 收到的音频字节数
}

type deviceStat struct {
	Frames    atomic.Int64
	Bytes     atomic.Int64
	LastLevel atomic.Int32
	LastFrame time.Time
}

func main() {
	// 子进程探测模式（见 probe.go）：必须在 flag.Parse 之前，
	// 因为 -probe-engine 不是标准 flag。
	if runEngineProbeIfRequested() {
		return
	}
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	s := &server{
		h:         hub.New(),
		turns:     newTurnLog(200),
		sessions:  make(map[string]*deviceStat),
		kws:       make(map[string]kwsState),
		play:      make(map[string]playInfo),
		startedAt: time.Now(),
	}
	s.h.OnRegister = func(sess *hub.Session) {
		log.Printf("设备上线: %s (%s) 型号=%s 固件=%s", sess.ID, sess.Remote, sess.Hello.Model, sess.Hello.Firmware)
	}
	s.h.OnUnregister = func(sess *hub.Session) {
		log.Printf("设备下线: %s", sess.ID)
		// 设备断了就把会话和音频管道一起清掉，避免重连后带着旧状态
		if s.dm != nil {
			if sl, ok := s.dm.Lookup(sess.ID); ok {
				sl.Idle()
			}
		}
		s.devMu.Lock()
		d := s.devAudio[sess.ID]
		delete(s.devAudio, sess.ID)
		s.devMu.Unlock()
		if d != nil {
			d.stop(deviceStopTimeout)
		}
	}

	// 配置先读出来：引擎与助手都按它初始化，改配置不用改命令行
	s.initConfig()
	// 先建助手档案再加载引擎：KWS 的关键词要从唤醒词生成，而唤醒词属于档案。
	// （引擎只由 setEngine 注入对话框，顺序反过来也安全，这里只是为了让唤醒词可用）
	s.initAgents()
	// TTS 路由层要在引擎之前建好：引擎装配完会把自己的 TTS 登记进来，
	// 这样"全局供应商"那一份实例不会被重复创建。
	s.initSpeech()
	// 加载本地音频引擎（VAD / ASR / TTS / KWS）
	if !*flagNoEngine {
		s.initEngine()
	}

	devMux := http.NewServeMux()
	devMux.HandleFunc("/ws", s.handleDeviceWS)
	devSrv := &http.Server{Addr: *flagDeviceAddr, Handler: devMux}

	adminMux := http.NewServeMux()
	s.routes(adminMux)
	adminSrv := &http.Server{Addr: *flagAdminAddr, Handler: adminMux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("设备面监听 %s/ws", *flagDeviceAddr)
		// 关键：监听 socket 也要关 Nagle。
		// 音频每 30~60ms 一帧，Nagle 会把小包攒到 ~40ms 才发，
		// 直接表现为"每帧多等 35ms"，几十帧累积就是好几秒。
		lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			}); err != nil {
				return err
			}
			return serr
		}}
		ln, err := lc.Listen(context.Background(), "tcp", *flagDeviceAddr)
		if err != nil {
			log.Fatalf("设备面监听失败: %v", err)
		}
		if err := devSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("设备面启动失败: %v", err)
		}
	}()
	go func() {
		log.Printf("管理面监听 %s", *flagAdminAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("管理面启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("收到退出信号，优雅关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = devSrv.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)
	for _, sess := range s.h.List() {
		sess.Close("server shutdown")
	}
	// 关掉备用 TTS 实例持有的 HTTP 长连接（云端供应商会 keep-alive 60 秒）
	if s.speech != nil {
		_ = s.speech.Close()
	}
}

// handleDeviceWS 处理音箱连接。
func (s *server) handleDeviceWS(w http.ResponseWriter, r *http.Request) {
	if want := s.deviceToken(); want != "" {
		got := r.Header.Get("Authorization")
		if got != "Bearer "+want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 设备端不校验 Origin（非浏览器）
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("接受连接失败: %v", err)
		return
	}
	conn.SetReadLimit(1 << 20)

	ctx := r.Context()

	// 第一帧必须是 hello
	typ, data, err := conn.Read(ctx)
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "read hello failed")
		return
	}
	if typ != websocket.MessageText {
		conn.Close(websocket.StatusPolicyViolation, "hello must be text")
		return
	}
	helloEnv, err := proto.ParseEnvelope(data)
	if err != nil || helloEnv.Type != proto.TypeHello {
		conn.Close(websocket.StatusPolicyViolation, "bad hello")
		return
	}
	var hp proto.HelloPayload
	if err := helloEnv.Decode(&hp); err != nil || hp.DeviceID == "" {
		conn.Close(websocket.StatusPolicyViolation, "bad hello payload")
		return
	}

	sess := hub.NewSession(hp.DeviceID, conn, hp, r.RemoteAddr)
	s.h.Register(sess)
	s.stat(sess.ID)
	s.startDeviceAudio(sess.ID)

	ack, _ := proto.NewResp(helloEnv.ID, proto.HelloAck{
		Protocol:     proto.Version,
		HeartbeatMS:  25000,
		StreamAudio:  true,
		FrameMS:      30,
		ServerTimeMS: proto.NowMS(),
	})
	ack.Type = proto.TypeHelloAck
	ack.Cmd = ""
	if err := sess.SendEnvelope(ctx, ack); err != nil {
		sess.Close("send ack failed")
		s.h.Unregister(sess)
		return
	}

	// 连上就把唤醒词对齐：设备重装、换词、恢复出厂都不用再登机器改文件。
	// 用独立协程，避免设备响应慢时卡住读循环。
	go s.syncKeywords(context.Background(), sess.ID)

	defer func() {
		s.h.Unregister(sess)
		sess.Close("connection closed")
	}()

	for {
		typ, data, err := sess.Conn.Read(ctx)
		if err != nil {
			// 静默 return 会让"为什么老掉线"完全没法查，必须留痕
			if ctx.Err() == nil {
				log.Printf("设备 %s 读失败，关闭连接: %v", sess.ID, err)
			}
			return
		}
		sess.Touch()

		switch typ {
		case websocket.MessageBinary:
			f, err := proto.ParseFrame(data)
			if err != nil {
				continue
			}
			s.onAudio(sess, f)
		case websocket.MessageText:
			env, err := proto.ParseEnvelope(data)
			if err != nil {
				continue
			}
			s.onEnvelope(sess, env)
		}
	}
}

func (s *server) stat(id string) *deviceStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sessions[id]
	if !ok {
		st = &deviceStat{}
		s.sessions[id] = st
	}
	return st
}

// onAudio 处理上行音频。P1 只做计数与音量，P2 接入 VAD/KWS/ASR。
func (s *server) onAudio(sess *hub.Session, f *proto.Frame) {
	if f.Type != proto.FrameAudioUp {
		return
	}
	s.frames.Add(1)
	s.bytes.Add(int64(len(f.Payload)))
	s.pushAudio(sess.ID, f.Payload)

	st := s.stat(sess.ID)
	st.Frames.Add(1)
	st.Bytes.Add(int64(len(f.Payload)))
	st.LastFrame = time.Now()

	// 抽样算峰值，供后台波形用（不做重活）
	if len(f.Payload) >= 2 {
		peak := 0
		for i := 0; i+1 < len(f.Payload); i += 64 { // 每 32 个采样取一个，够画波形了
			v := int(int16(uint16(f.Payload[i]) | uint16(f.Payload[i+1])<<8))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		st.LastLevel.Store(int32(peak * 100 / 32768))
	}
}

func (s *server) onEnvelope(sess *hub.Session, e *proto.Envelope) {
	switch e.Type {
	case proto.TypeResp:
		if !sess.Deliver(e) {
			log.Printf("收到无主应答 id=%s cmd=%s", e.ID, e.Cmd)
		}
	case proto.TypePong:
		// 心跳回包，Touch 已经刷新过
	case proto.TypeEvent:
		switch e.Cmd {
		case proto.EvAudio:
			var lv proto.AudioLevel
			if e.Decode(&lv) == nil {
				s.stat(sess.ID).LastLevel.Store(int32(lv.Level))
			}
		case proto.EvPlaying:
			var st proto.PlayingState
			_ = e.Decode(&st)
			s.setPlaying(sess.ID, st.State)
			log.Printf("设备 %s 播放状态: %s", sess.ID, st.State)
		case proto.EvKWS:
			var kw proto.KWSState
			if e.Decode(&kw) == nil && kw.Keyword != "" {
				s.wakeByKeyword(sess.ID, kw.Keyword)
			}
		case proto.EvFatal:
			var fe proto.FatalEvent
			_ = e.Decode(&fe)
			log.Printf("设备 %s 致命错误: %s", sess.ID, fe.Reason)
		}
	}
}

// routes 注册管理面路由。
func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("POST /api/devices/{id}/tone", s.handleTone)
	mux.HandleFunc("POST /api/devices/{id}/record/start", s.handleRecordStart)
	mux.HandleFunc("POST /api/devices/{id}/record/stop", s.handleRecordStop)
	mux.HandleFunc("POST /api/devices/{id}/ping", s.handlePing)
	mux.HandleFunc("POST /api/devices/{id}/shell", s.handleShell)
	mux.HandleFunc("GET /api/agents", s.handleAgents)
	mux.HandleFunc("GET /api/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /api/settings", s.handleSettingsPut)
	mux.HandleFunc("GET /api/turns", s.handleTurns)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("GET /api/recordings", s.handleRecordings)
	mux.HandleFunc("GET /api/recordings/{name}", s.handleRecordingFile)
	mux.HandleFunc("DELETE /api/turns", s.handleTurnsClear)
	mux.HandleFunc("POST /api/engine/reload", s.handleEngineReload)
	mux.HandleFunc("POST /api/tools/test", s.handleToolsTest)
	mux.HandleFunc("POST /api/tools/test-search", s.handleTestSearch)
	mux.HandleFunc("POST /api/tools/test-weather", s.handleTestWeather)
	mux.HandleFunc("GET /api/kws", s.handleKWSStatus)
	mux.HandleFunc("POST /api/kws/sync", s.handleKWSSync)
	mux.HandleFunc("POST /api/debug/wake", s.handleDebugWake)
	mux.HandleFunc("POST /api/debug/ask", s.handleDebugAsk)
	mux.HandleFunc("POST /api/engine/tts", s.handleEngineTTS)
	mux.HandleFunc("POST /api/engine/asr", s.handleEngineASR)
	// 多供应商 TTS：供应商声明、音色清单、试听、声音复刻
	mux.HandleFunc("GET /api/tts/providers", s.handleTTSProviders)
	mux.HandleFunc("GET /api/tts/voices", s.handleTTSVoices)
	mux.HandleFunc("POST /api/tts/preview", s.handleTTSPreview)
	mux.HandleFunc("POST /api/tts/clone", s.handleTTSClone)
	mux.HandleFunc("GET /api/tts/clone", s.handleTTSCloneStatus)
	mux.Handle("GET /", web.Handler())
}

// config 返回当前配置快照。
func (s *server) config() store.Settings {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *server) setConfig(st store.Settings) {
	s.cfgMu.Lock()
	s.cfg = st
	s.cfgMu.Unlock()
}

// deviceToken 返回设备鉴权 token：后台配置优先，命令行兜底。
func (s *server) deviceToken() string {
	if t := s.config().Server.DeviceToken; t != "" {
		return t
	}
	return *flagToken
}

// engineCfg 把配置段翻译成引擎参数。
func (s *server) engineCfg(st store.Settings) audio.SherpaConfig {
	e := st.Engine
	cfg := audio.SherpaConfig{
		ModelDir:      e.ModelDir,
		ASRDir:        e.ASRDir,
		ASRKind:       e.ASRKind,
		ASRLang:       e.ASRLang,
		TTSModel:      e.TTSModel,
		TTSKind:       e.TTSKind,
		TTSVocoder:    e.TTSVocoder,
		Threads:       e.Threads,
		Debug:         *flagEngineDebug,
		VADThreshold:  e.VADThreshold,
		KWSEnabled:    e.KWSEnabled,
		KWSScore:      e.KWSScore,
		KWSThreshold:  e.KWSThreshold,
		VADMinSilence: float32(st.Dialog.SilenceMS) / 1000,
		VADMinSpeech:  float32(st.Dialog.MinSpeechMS) / 1000,
	}
	// 命令行显式指定路径时以命令行为准（便于临时排查）
	if *flagModels != "" {
		cfg.ModelDir = *flagModels
	}
	if *flagASRDir != "" {
		cfg.ASRDir = *flagASRDir
	}
	if *flagTTSModel != "" {
		cfg.TTSModel = *flagTTSModel
	}
	return cfg
}

// initSpeech 建 TTS 路由层。
//
// 它必须在 initEngine 之前：引擎装配完会把"自己装了哪个供应商、用哪份参数"
// 登记进来，路由层据此决定要不要为全局供应商另建实例。
func (s *server) initSpeech() {
	st := s.config()
	defProv := st.Engine.TTSProvider
	if defProv == "" {
		defProv = "sherpa"
	}
	s.speech = audio.NewService(defProv, st.Engine.TTSProviders[defProv])
	// 在线音色清单缓存放在数据目录旁边：独立小文件，不进 settings.json
	if s.store != nil {
		s.speech.SetVoiceCacheDir(filepath.Join(filepath.Dir(s.store.Path()), "tts-voices"))
	}
	s.speech.SetSpeed(st.Voice.Speed)
	s.speech.Logf = func(format string, args ...any) { log.Printf(format, args...) }
	if s.dm != nil {
		s.dm.SetSpeech(s.speech)
	}
}

// loadEngine 按配置构造引擎。
//
// 装配方式：VAD / ASR / TTS / KWS 各自按「供应商名」去注册表取零件，
// 再拼成一个 Bundle。这样做的好处是：
//   - 接新供应商不用改这个函数（新供应商自己 init 注册）
//   - 某个零件坏了不拖累其它零件：TTS 模型填错，识别照常工作
//   - 每个零件独立一把锁（见 audio/engine_sherpa.go），合成不再卡住唤醒
//
// 返回的第二个值是"引擎里实际装的是哪个 TTS 供应商"：路由层要靠它判断
// 某次请求能否直接复用引擎里的实例（配置写 doubao、实际回退成 sherpa 时，
// 这个值和配置里的不一样，不能拿配置去猜）。
func (s *server) loadEngine(st store.Settings) (audio.Engine, string, error) {
	cfg := s.engineCfg(st).Resolved()
	// KWS 的关键词文件是引擎构造时读进去的，所以必须先生成再加载。
	// 模型目录通常是只读挂载，关键词写到数据目录。
	if cfg.KWSEnabled {
		if path, words, errs, err := s.prepareKWSKeywords(cfg); err != nil {
			// 关键词生成失败不该拖垮整个引擎：记下来，退回"识别文本唤醒"
			log.Printf("KWS 关键词生成失败，本次不启用 KWS: %v", err)
			cfg.KWSEnabled = false
		} else {
			cfg.KeywordsFile = path
			for _, e := range errs {
				log.Printf("KWS 忽略唤醒词: %v", e)
			}
			log.Printf("KWS 关键词已就绪: %v → %s", words, path)
		}
	}
	// 第一层：便宜的文件自检。坏文件（缺失 / LFS 指针 / 残片）在这里就拦下，
	// 给出「哪个文件、为什么、怎么修」的说明。
	if err := audio.ValidateModelFiles(cfg); err != nil {
		return nil, "", err
	}

	asrName := resolveProvider(st.Engine.ASRProvider, "ASR", audio.HasASRProvider)
	ttsName := resolveProvider(st.Engine.TTSProvider, "TTS", audio.HasTTSProvider)

	// 第二层：把真正的模型构造放到子进程里试一次。
	// sherpa 是 C++ 的，坏模型会 abort() 杀掉整个进程，只有子进程能兜住。
	// 同一套路径探测通过过就不再重复探测，避免每次重启都多花 7 秒。
	// 非本地供应商（云端 API）没有 C++ 模型可崩，跳过探测。
	fp := ""
	if asrName == "sherpa" || ttsName == "sherpa" {
		var err error
		fp, err = s.probeEngine(cfg)
		if err != nil {
			return nil, "", err
		}
	}

	parts := audio.BundleParts{Speed: st.Voice.Speed, ASRName: "off", TTSName: "off"}
	// 实际装上的 TTS 供应商（可能因为失败回退成 sherpa，也可能干脆没起来）
	loadedTTS := "none"

	// VAD 是必需件：没有它整条链路没法分段，直接报错让人去修。
	vad, err := audio.NewVoiceDetector("silero", audio.VADOptions{
		Model:      cfg.VADModel,
		Threshold:  cfg.VADThreshold,
		MinSilence: cfg.VADMinSilence,
		MinSpeech:  cfg.VADMinSpeech,
	})
	if err != nil {
		return nil, "", err
	}
	parts.VAD = vad

	// 其余零件允许单独失败：记下原因，其余照常工作。
	// 本地供应商必须有模型路径；云端供应商（如 http）不需要。
	asrOpt := audio.ASROptions{
		Dir: cfg.ASRDir, Kind: cfg.ASRKind, Lang: cfg.ASRLang,
		Threads: cfg.Threads, Debug: cfg.Debug,
		Params: st.Engine.ASRParams,
	}
	if asrName != "none" && (cfg.ASRDir != "" || !audio.ASRProviderLocal(asrName)) {
		asr, err := audio.NewRecognizer(asrName, asrOpt)
		// 云端供应商配错（地址写错、服务没起）时回退本地模型：
		// 让整个助手哑掉，比"少一个功能"糟糕得多。
		if err != nil && asrName != "sherpa" && cfg.ASRDir != "" && audio.HasASRProvider("sherpa") {
			log.Printf("ASR 供应商 %s 不可用（%v），回退到 sherpa", asrName, err)
			asr, err = audio.NewRecognizer("sherpa", asrOpt)
		}
		if err != nil {
			log.Printf("ASR 未加载（%s）: %v", asrName, err)
			parts.ASRName = "error"
		} else {
			parts.ASR, parts.ASRName = asr, asr.Status()
		}
	}
	ttsOpt := audio.TTSOptions{
		ModelDir: cfg.ModelDir,
		Model:    cfg.TTSModel, Kind: cfg.TTSKind, Vocoder: cfg.TTSVocoder,
		Tokens: cfg.TTSTokens, Lexicon: cfg.TTSLexicon,
		DataDir: cfg.TTSDataDir, DictDir: cfg.TTSDictDir,
		Threads: cfg.Threads, Debug: cfg.Debug, Speed: st.Voice.Speed,
		Params: st.Engine.TTSParams,
	}
	if ttsName != "none" && (cfg.TTSModel != "" || !audio.TTSProviderLocal(ttsName)) {
		tts, err := audio.NewSynthesizer(ttsName, ttsOpt)
		if err != nil && ttsName != "sherpa" && cfg.TTSModel != "" && audio.HasTTSProvider("sherpa") {
			log.Printf("TTS 供应商 %s 不可用（%v），回退到 sherpa", ttsName, err)
			tts, err = audio.NewSynthesizer("sherpa", ttsOpt)
			ttsName = "sherpa"
		}
		if err != nil {
			log.Printf("TTS 未加载（%s）: %v", ttsName, err)
			parts.TTSName = "error"
			loadedTTS = "error"
		} else {
			parts.TTS, parts.TTSName = tts, tts.Status()
			loadedTTS = ttsName
		}
	}
	if cfg.KWSEnabled {
		kws, err := audio.NewKeywordDetector("sherpa", audio.KWSOptions{
			Encoder: cfg.KWSEncoder, Decoder: cfg.KWSDecoder, Joiner: cfg.KWSJoiner,
			Tokens: cfg.KWSTokens, KeywordsFile: cfg.KeywordsFile,
			Score: cfg.KWSScore, Threshold: cfg.KWSThreshold,
		})
		if err != nil {
			log.Printf("KWS 未加载: %v", err)
		} else {
			parts.KWS = kws
		}
	}

	eng := audio.NewBundle(parts)
	// 一个零件都没起来，等于引擎不可用：报错让后台看得见。
	if !eng.HasASR() && !eng.HasTTS() {
		_ = eng.Close()
		return nil, "", fmt.Errorf("ASR 与 TTS 都没有加载成功，请检查模型配置（%s）", eng.Status())
	}
	// 真正加载成功了才记住指纹，否则下次还要继续探测
	s.markEngineOK(fp)
	return eng, loadedTTS, nil
}

// resolveProvider 把配置里的供应商名变成"真实可用的注册名"。
//
// 配了但本构建没装（比如 Mac 开发机构建里没有 sherpa）时不全盘失败，
// 而是回退到 sherpa/none 并打日志：音箱还能说话，比整机静音好。
func resolveProvider(configured, kind string, exists func(string) bool) string {
	name := strings.ToLower(strings.TrimSpace(configured))
	if name == "" {
		// 没配就用 sherpa（本构建有的话），否则关闭这项能力
		if exists("sherpa") {
			return "sherpa"
		}
		return "none"
	}
	if exists(name) {
		return name
	}
	log.Printf("%s 供应商 %q 未在本构建中注册，回退", kind, name)
	if exists("sherpa") {
		return "sherpa"
	}
	return "none"
}

// initEngine 加载 sherpa 引擎。失败不退出：链路仍可用，后台能看到错误原因。
func (s *server) initEngine() {
	eng, ttsProv, err := s.loadEngine(s.config())
	if err != nil {
		s.setEngineErr(err)
		log.Printf("音频引擎未加载: %v", err)
		return
	}
	s.installEngineWith(eng, ttsProv)
	log.Printf("音频引擎已加载: %s", eng.Status())
}

// installEngine 换上新引擎（不知道引擎里的 TTS 是哪套配置，按当前设置推断）。
func (s *server) installEngine(eng audio.Engine) {
	s.installEngineWith(eng, "")
}

// installEngineWith 换上新引擎：旧的标记退休（等使用者放手才真正 Close）。
//
// ttsProv 是"引擎里实际装上的 TTS 供应商"（loadEngine 的返回值）。传空表示
// 由当前配置推断——只为兼容"只想换个引擎、不关心 TTS 归属"的调用方（如测试）。
func (s *server) installEngineWith(eng audio.Engine, ttsProv string) {
	if eng == nil {
		return
	}
	h := newEngineHandle(eng)

	s.engMu.Lock()
	old := s.engine
	oldDM := s.dmEngine
	s.engine = h
	s.dmEngine = nil
	s.engineErr = nil
	s.engMu.Unlock()

	// 告诉路由层"引擎里装的是谁"，它才能判断某次请求能否直接复用这份实例
	// （复用 = 不必为全局供应商再建一个一模一样的 TTS）。
	if s.speech != nil {
		st := s.config()
		prov := ttsProv
		if prov == "" {
			prov = resolveProvider(st.Engine.TTSProvider, "TTS", audio.HasTTSProvider)
		}
		s.speech.SetDefault(st.Engine.TTSProvider, st.Engine.TTSProviders[st.Engine.TTSProvider])
		s.speech.SetEngine(eng, prov, st.Engine.TTSProviders[prov])
	}

	// 对话框长期持有引擎（每句话合成都用它），这里给它一份引用，
	// 并记住"它替谁持着"，换引擎时才能把旧的那份准确还回去。
	if s.dm != nil {
		if e, ok := h.acquire(); ok {
			s.dm.SetEngine(e)
			s.engMu.Lock()
			s.dmEngine = h
			s.engMu.Unlock()
		}
	}
	if oldDM != nil && oldDM != h {
		oldDM.release() // 归还对话框那份
	}
	if old != nil && old != h {
		old.retire() // 归还服务端那份；引用归零才真正 Close
	}

	// 设备管道里存着"建管道那一刻"的引擎指针，必须跟着重建：
	// 一是避免继续用旧引擎，二是让引擎刚就绪时就登记过、还没管道的设备恢复。
	s.resetDeviceAudio()
}

// setEngineErr 只记录错误，不动正在工作的引擎。
// 填错一个模型路径不该让音箱当场失声。
func (s *server) setEngineErr(err error) {
	s.engMu.Lock()
	s.engineErr = err
	s.engMu.Unlock()
}

// autoReloadEngine 后台重建引擎（唤醒词变更后调用，让 KWS 立刻吃到新词）。
//
// 为什么必须自动做：以前只在前端弹"点重载后生效"，但没人会守着后台点按钮，
// 结果就是"档案里加了新唤醒词、音箱文件也同步了，喊它却没反应"——
// 服务端 KWS 的 keywords.txt 是构造 keyword spotter 时读进去的，不重建引擎
// 就一直是旧词（实测「年糕」喊不醒，重启服务端后立刻能唤醒）。
//
// 重载要 4~12 秒，期间识别/唤醒会短暂不可用；连着改几次唤醒词时合并成
// 最后一次重载。
func (s *server) autoReloadEngine() {
	s.reloadMu.Lock()
	if s.reloadBusy {
		s.reloadAgain = true
		s.reloadMu.Unlock()
		return
	}
	s.reloadBusy = true
	s.reloadMu.Unlock()

	for {
		st := s.config()
		start := time.Now()
		eng, ttsProv, err := s.loadEngine(st)
		if err != nil {
			// 保留正在工作的旧引擎，只记错误（与手动重载同一语义）
			s.setEngineErr(err)
			log.Printf("唤醒词变更后自动重载引擎失败（继续用旧引擎）: %v", err)
		} else {
			s.installEngineWith(eng, ttsProv)
			log.Printf("唤醒词变更，引擎已自动重载: %s (%dms)", eng.Status(), time.Since(start).Milliseconds())
		}

		s.reloadMu.Lock()
		if !s.reloadAgain {
			s.reloadBusy = false
			s.reloadMu.Unlock()
			return
		}
		s.reloadAgain = false
		s.reloadMu.Unlock()
	}
}

// getEngine 返回引擎指针（不加引用）：只给"看一眼状态"用。
// 要真正跑推理必须用 acquireEngine。
func (s *server) getEngine() audio.Engine {
	s.engMu.RLock()
	h := s.engine
	s.engMu.RUnlock()
	if h == nil {
		return nil
	}
	return h.raw()
}

func (s *server) engineError() error {
	s.engMu.RLock()
	defer s.engMu.RUnlock()
	return s.engineErr
}

func (s *server) engineState() string {
	if *flagNoEngine {
		return "disabled"
	}
	if err := s.engineError(); err != nil {
		return "error: " + err.Error()
	}
	if s.getEngine() != nil {
		return "ready"
	}
	return "unknown"
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":           true,
		"engine":       s.engineState(),
		"version":      serverVersion,
		"uptime_sec":   int64(time.Since(s.startedAt).Seconds()),
		"devices":      s.h.Count(),
		"audio_frames": s.frames.Load(),
		"audio_bytes":  s.bytes.Load(),
		"audio_kbps":   s.kbps(),
	})
}

// kbps 返回最近的上行码率（按进程启动以来的平均，先给个可用值）。
func (s *server) kbps() float64 {
	el := time.Since(s.startedAt).Seconds()
	if el < 1 {
		return 0
	}
	return float64(s.bytes.Load()) * 8 / el / 1000
}

func (s *server) handleDevices(w http.ResponseWriter, r *http.Request) {
	list := s.h.List()
	out := make([]map[string]any, 0, len(list))
	for _, sess := range list {
		st := s.stat(sess.ID)
		last := st.LastFrame
		var lastAgo int64 = -1
		if !last.IsZero() {
			lastAgo = int64(time.Since(last).Milliseconds())
		}
		out = append(out, map[string]any{
			"id":                sess.ID,
			"model":             sess.Hello.Model,
			"firmware":          sess.Hello.Firmware,
			"version":           sess.Hello.Version,
			"caps":              sess.Hello.Caps,
			"remote":            sess.Remote,
			"online_sec":        int64(time.Since(sess.Since).Seconds()),
			"last_seen_s":       int64(time.Since(sess.LastSeen()).Seconds()),
			"level":             st.LastLevel.Load(),
			"frames":            st.Frames.Load(),
			"bytes":             st.Bytes.Load(),
			"last_audio_ms_ago": lastAgo,
		})
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

func (s *server) handleTone(w http.ResponseWriter, r *http.Request) {
	sess, err := s.h.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "设备离线"})
		return
	}
	var p proto.TonePayload
	_ = json.NewDecoder(r.Body).Decode(&p)
	if p.FreqHz == 0 {
		p = proto.TonePayload{FreqHz: 880, DurationMS: 160, Amplitude: 9000}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := sess.Request(ctx, proto.CmdPlayTone, p, 3*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"rtt_ms":      time.Since(start).Milliseconds(),
		"device_resp": resp.Msg,
	})
}

func (s *server) handleRecordStart(w http.ResponseWriter, r *http.Request) {
	sess, err := s.h.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "设备离线"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := sess.Request(ctx, proto.CmdRecordStart, proto.RecordStartPayload{
		SampleRate: 16000, FrameMS: 30, Channels: 1,
	}, 3*time.Second); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	sess, err := s.h.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "设备离线"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := sess.Request(ctx, proto.CmdRecordStop, nil, 3*time.Second); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	sess, err := s.h.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "设备离线"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := sess.Request(ctx, proto.CmdDeviceInfo, nil, 3*time.Second); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "rtt_ms": time.Since(start).Milliseconds()})
}

func (s *server) handleShell(w http.ResponseWriter, r *http.Request) {
	sess, err := s.h.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "设备离线"})
		return
	}
	var p proto.ShellPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Script == "" {
		writeJSON(w, 400, map[string]any{"error": "需要 script 字段"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	resp, err := sess.Request(ctx, proto.CmdShellRun, p, 12*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	var res proto.ShellResult
	_ = resp.Decode(&res)
	writeJSON(w, 200, map[string]any{"ok": true, "result": res})
}

// handleIndex 是 P1 的临时调试页，P4 会换成 Vue 构建产物。
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, indexHTML, serverVersion, s.h.Count())
}

const indexHTML = `<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<title>xiaoai-go</title>
<style>
 body{background:#0d1117;color:#e6edf3;font:14px/1.6 system-ui,sans-serif;margin:0;padding:40px}
 .card{max-width:720px;margin:0 auto;background:#161b22;border:1px solid #30363d;border-radius:12px;padding:24px}
 h1{font-size:18px;margin:0 0 4px}
 .sub{color:#8b949e;font-size:13px;margin-bottom:20px}
 button{background:#1D9E75;color:#fff;border:0;border-radius:8px;padding:10px 16px;font-size:14px;cursor:pointer}
 button:disabled{background:#30363d;color:#8b949e;cursor:not-allowed}
 pre{background:#0d1117;border:1px solid #30363d;border-radius:8px;padding:12px;overflow:auto;font-size:12px}
 .row{display:flex;gap:12px;align-items:center;margin-bottom:12px}
</style>
<div class="card">
 <h1>xiaoai-go 服务端 <span class="sub">v%s</span></h1>
 <div class="sub">在线设备 %d 台 · P1 阶段：链路自检</div>
 <div class="row">
   <button id="ping">读取设备信息</button>
   <button id="tone">播放测试音</button>
   <button id="rec">开始录音</button>
   <button id="stop">停止录音</button>
 </div>
 <pre id="out">点上面的按钮试试</pre>
</div>
<script>
const out = document.getElementById('out');
let deviceId = null;
async function api(path, opts) {
  const r = await fetch(path, opts);
  const t = await r.text();
  out.textContent = path + ' → ' + r.status + '\n' + t;
  return r;
}
async function firstDevice() {
  if (deviceId) return deviceId;
  const r = await fetch('/api/devices');
  const d = await r.json();
  if (!d.devices || !d.devices.length) { out.textContent = '没有设备在线'; return null; }
  deviceId = d.devices[0].id;
  out.textContent = '设备: ' + JSON.stringify(d.devices[0], null, 2);
  return deviceId;
}
document.getElementById('ping').onclick = async () => {
  const id = await firstDevice(); if (!id) return;
  await api('/api/devices/' + id + '/ping', {method:'POST'});
};
document.getElementById('tone').onclick = async () => {
  const id = await firstDevice(); if (!id) return;
  await api('/api/devices/' + id + '/tone', {method:'POST', body:'{}'});
};
document.getElementById('rec').onclick = async () => {
  const id = await firstDevice(); if (!id) return;
  await api('/api/devices/' + id + '/record/start', {method:'POST'});
};
document.getElementById('stop').onclick = async () => {
  const id = await firstDevice(); if (!id) return;
  await api('/api/devices/' + id + '/record/stop', {method:'POST'});
};
</script></html>`

// envOr 返回命令行值，为空时回落到环境变量。
// 凭据走环境变量，避免出现在命令行（进程列表/日志可见）。
func envOr(flagVal, envName string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envName)
}

// initConfig 打开可持久化配置。首次启动时用命令行参数生成默认值。
//
// 配置读不出来就不硬跑：没有助手档案的话，设备连上来也只会沉默，
// 不如直接退出，让人看到明确的原因。
func (s *server) initConfig() {
	def := store.Defaults()
	def.Profiles = profilesFromFlags()

	// 命令行/环境变量给了 HA 参数就当作默认值（之后后台可改）
	haURL := envOr(*flagHAURL, "XIAOAI_HA_URL")
	haToken := envOr(*flagHAToken, "XIAOAI_HA_TOKEN")
	if haURL != "" {
		def.Tools.HAURL = haURL
		def.Tools.HAToken = haToken
	}
	if *flagToken != "" {
		def.Server.DeviceToken = *flagToken
	}
	if *flagModels != "" {
		def.Engine.ModelDir = *flagModels
	}

	st, err := store.Open(*flagSettings, def)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}
	s.store = st
	cur := st.Get()
	s.setConfig(cur)
	s.turns.SetMax(cur.Server.KeepTurns)
	log.Printf("配置文件: %s", st.Path())
}

// initAgents 装配档案表、工具集与对话管理器。
func (s *server) initAgents() {
	if s.store == nil {
		return
	}
	if err := s.applySettings(s.config()); err != nil {
		log.Printf("配置应用失败: %v", err)
		return
	}

	s.dm = dialog.NewManager(s.reg.Load(), s.toolsSnapshot(), s.getEngine(), log.Default())
	// 固定应答缓存：唤醒/退出/出错这几句反复出现，合成一次就够了
	s.initReplyCache()
	s.applyDialogConfig(s.config())
	go s.dm.Sweep(context.Background(), 2*time.Second)
	s.dm.OnTurn = func(t *dialog.Turn) {
		s.turns.add(t)
		s.devMu.Lock()
		s.lastTurn = t
		s.devMu.Unlock()
	}
	s.devAudio = make(map[string]*deviceAudio)

	for _, p := range s.config().Profiles {
		log.Printf("助手档案: %s(%s) 唤醒词=%v 模型=%s 工具=%v",
			p.ID, p.Name, p.WakeWords, p.LLM.Model, p.Tools)
	}
}

// profilesFromFlags 用命令行参数生成一个默认档案。
func profilesFromFlags() []*agent.Profile {
	if *flagProfiles != "" {
		if ps, err := agent.LoadFile(*flagProfiles); err == nil && len(ps) > 0 {
			return ps
		} else if err != nil {
			log.Printf("读取 -profiles 失败，改用命令行参数: %v", err)
		}
	}

	base := envOr(*flagLLMBase, "XIAOAI_LLM_BASE")
	key := envOr(*flagLLMKey, "XIAOAI_LLM_KEY")
	model := envOr(*flagLLMModel, "XIAOAI_LLM_MODEL")
	if base == "" || model == "" {
		base, model = "https://tokenhub.tencentmaas.com/v1", "deepseek/deepseek-flash"
	}
	words := []string{}
	for _, w := range strings.Split(*flagWakeWords, ",") {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, w)
		}
	}
	return []*agent.Profile{{
		ID: "default", Name: "管家", WakeWords: words,
		SystemPrompt: defaultSystemPrompt,
		LLM: agent.LLM{
			BaseURL: base, APIKey: key, Model: model,
			Extra: map[string]any{"enable_thinking": false},
		},
	}}
}

const defaultSystemPrompt = `你是一个家庭语音助手，通过小爱音箱和主人对话。
说话要求：
- 中文口语，简短，一般不超过 60 字，因为回答会被念出来
- 不要用 markdown、列表、代码块、表情符号
- 提示词里的 [已知信息] 已经给了当前时间、天气和相关设备状态，直接用这些回答，不要为它们调用工具
- 只有真要操作具体设备时才调工具，一次拿到结果就用一句话回答，不要反复查
- 搜索到多个候选时挑最贴近的直接执行，不要反问主人
- 涉及锁、报警器、阀门这类设备，直接婉拒`

// startDeviceAudio 为设备启动音频处理协程（每台设备一次）。
func (s *server) startDeviceAudio(id string) {
	if s.dm == nil {
		return
	}
	// 借一份引擎引用：管道活多久，引用就持多久。
	// 这样换引擎时旧引擎不会被立刻 Close，等管道停下才关。
	eng, release, _ := s.acquireEngine()
	if eng == nil {
		// 引擎没就绪也先把设备登记进去：等引擎加载成功（installEngine →
		// resetDeviceAudio）会重新建管道。旧实现在这里直接 return，
		// 结果音箱必须断开重连才能恢复。
		release = func() {}
		log.Printf("[%s] 引擎未就绪，音频管道待启用", id)
	}
	s.devMu.Lock()
	if _, ok := s.devAudio[id]; ok {
		s.devMu.Unlock()
		release()
		return
	}
	d := newDeviceAudio(id, s.h, eng, release, s.dm, s.deviceOptions(), func() bool {
		return s.config().Server.FreeTalk
	}, func() bool { return s.deviceMuted(id) }, func() bool {
		return s.config().Voice.AckTone
	}, func() bool {
		return s.config().Server.WakeByASR
	}, func() []string {
		return s.wakeWords()
	})
	s.devAudio[id] = d
	s.devMu.Unlock()
	d.start(context.Background())
}

func (s *server) pushAudio(id string, pcm []byte) {
	s.devMu.Lock()
	d := s.devAudio[id]
	s.devMu.Unlock()
	if d != nil {
		d.Push(pcm)
	}
}

func (s *server) session(id string) (*dialog.Session, error) {
	sess, err := s.h.Get(id)
	if err != nil {
		return nil, err
	}
	return s.dm.Session(id, &deviceTarget{hub: s.h, id: id, sess: sess}), nil
}

func (s *server) wakeByKeyword(id, keyword string) {
	if s.dm == nil {
		return
	}
	dlg, err := s.session(id)
	if err != nil {
		return
	}
	if err := dlg.Wake(context.Background(), keyword); err != nil {
		log.Printf("[%s] 唤醒失败: %v", id, err)
	}
}

// wakeWords 是给设备管道用的回调：每次现取，改唤醒词立刻生效。
func (s *server) wakeWords() []string {
	if r := s.reg.Load(); r != nil {
		return r.WakeWords()
	}
	return nil
}

// toolsSnapshot 取当前工具注册表（toolsMu 保护的是指针替换）。
func (s *server) toolsSnapshot() *tools.Registry {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.tools
}

func (s *server) handleAgents(w http.ResponseWriter, r *http.Request) {
	r0 := s.reg.Load()
	if r0 == nil {
		writeJSON(w, 503, map[string]any{"error": "档案未加载"})
		return
	}
	out := make([]map[string]any, 0)
	for _, p := range r0.All() {
		out = append(out, map[string]any{
			"id": p.ID, "name": p.Name, "wake_words": p.WakeWords,
			"model": p.LLM.Model, "tools": p.Tools,
		})
	}
	writeJSON(w, 200, map[string]any{"agents": out})
}

// handleDebugWake 模拟一次唤醒（没有音箱时测试路由用）。
func (s *server) handleDebugWake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device  string `json:"device"`
		Keyword string `json:"keyword"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Device == "" || req.Keyword == "" {
		writeJSON(w, 400, map[string]any{"error": "需要 device 与 keyword"})
		return
	}
	dlg, err := s.session(req.Device)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	if err := dlg.Wake(r.Context(), req.Keyword); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "profile": dlg.ProfileID(), "state": dlg.State()})
}

// handleDebugAsk 模拟一次识别结果，返回本轮的可观测数据。
func (s *server) handleDebugAsk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device string `json:"device"`
		Text   string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Device == "" || req.Text == "" {
		writeJSON(w, 400, map[string]any{"error": "需要 device 与 text"})
		return
	}
	dlg, err := s.session(req.Device)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	// 与真实链路一致：没唤醒就不回应。调试时可加 force=1 绕过。
	if !dlg.Active() && r.URL.Query().Get("force") != "1" {
		writeJSON(w, 409, map[string]any{"ignored": true, "reason": "未唤醒，已忽略"})
		return
	}
	turn, err := dlg.HandleText(r.Context(), req.Text)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "turn": turn})
		return
	}
	writeJSON(w, 200, map[string]any{"turn": turn})
}

// handleEngineTTS 合成一段语音，返回 WAV，便于人耳验证音色。
//
// 走路由层而不是引擎：这样"试听某个还没保存的音色"也能验证，
// 后台选完音色可以先听再存。
func (s *server) handleEngineTTS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text     string  `json:"text"`
		Provider string  `json:"provider"`
		Voice    string  `json:"voice"`
		Speed    float32 `json:"speed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeJSON(w, 400, map[string]any{"error": "需要 text 字段"})
		return
	}
	if s.speech == nil {
		writeJSON(w, 503, map[string]any{"error": "语音服务未初始化: " + s.engineState()})
		return
	}
	start := time.Now()
	choice := audio.VoiceChoice{Provider: req.Provider, Voice: req.Voice, Speed: req.Speed}
	pcm, rate, err := s.speech.Synthesize(r.Context(), req.Text, choice)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	plan := s.speech.Resolve(choice)
	writeWAV(w, pcm, rate)
	log.Printf("TTS: %q → %s/%s %d 字节 %.0fms @%dHz 耗时 %dms",
		req.Text, plan.Provider, plan.Voice, len(pcm),
		float64(len(pcm))/2/float64(rate)*1000, rate, time.Since(start).Milliseconds())
}

// handleEngineASR 识别原始 PCM（16k / S16LE / 单声道），返回文本。
func (s *server) handleEngineASR(w http.ResponseWriter, r *http.Request) {
	eng, release, ok := s.acquireEngine()
	if !ok {
		writeJSON(w, 503, map[string]any{"error": "引擎未就绪: " + s.engineState()})
		return
	}
	defer release()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil || len(body) < 3200 {
		writeJSON(w, 400, map[string]any{"error": "需要至少 0.1 秒的 16k PCM 原始数据"})
		return
	}
	start := time.Now()
	text, err := eng.Transcribe(r.Context(), body)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"text":   text,
		"ms":     time.Since(start).Milliseconds(),
		"pcm_ms": len(body) / 2 * 1000 / 16000,
	})
}

// writeWAV 输出 16bit 单声道 WAV。
func writeWAV(w http.ResponseWriter, pcm []byte, rate int) {
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", "inline; filename=\"tts.wav\"")
	var h [44]byte
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+len(pcm)))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:24], 1) // mono
	binary.LittleEndian.PutUint32(h[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(h[32:34], 2)
	binary.LittleEndian.PutUint16(h[34:36], 16)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(len(pcm)))
	w.Write(h[:])
	w.Write(pcm)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var _ = os.Getenv
