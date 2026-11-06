package proto

// 服务端 → 音箱端 的命令名。
const (
	CmdDeviceInfo  = "device.info"
	CmdRecordStart = "record.start"
	CmdRecordStop  = "record.stop"
	CmdPlayStart   = "play.start"
	CmdPlayStop    = "play.stop"
	CmdPlayTone    = "play.tone"
	CmdVolumeSet   = "volume.set"
	CmdShellRun    = "shell.run"
	CmdKWSSync     = "kws.sync" // 下发唤醒词清单，由设备落盘
	CmdSay         = "say"      // 让音箱用原生 TTS 念一段文字
	CmdSayStop     = "say.stop" // 立刻停止当前播报并清空队列
)

// 音箱端 → 服务端 的事件名。
const (
	EvAudio       = "event.audio"
	EvPlaying     = "event.playing"
	EvKWS         = "event.kws"
	EvInstruction = "event.instruction"
	EvFatal       = "event.fatal"
)

// HelloPayload 是客户端握手请求。
type HelloPayload struct {
	DeviceID string   `json:"device_id"`
	Model    string   `json:"model"`
	Firmware string   `json:"firmware"`
	Version  string   `json:"version"`  // client 版本
	Protocol int      `json:"protocol"` // 客户端支持的协议版本
	Caps     []string `json:"caps"`     // 能力：record/play/shell/kws/instruction
}

// HelloAck 是服务端握手应答。
type HelloAck struct {
	Protocol     int   `json:"protocol"`
	HeartbeatMS  int   `json:"heartbeat_ms"` // 期望的心跳间隔
	StreamAudio  bool  `json:"stream_audio"` // 是否需要连续音频流
	FrameMS      int   `json:"frame_ms"`     // 期望上行帧长（毫秒）
	ServerTimeMS int64 `json:"server_time_ms"`
}

// RecordStartPayload 启动麦克风采集。
type RecordStartPayload struct {
	Device     string `json:"device,omitempty"` // ALSA 设备，空则用客户端默认
	SampleRate int    `json:"sample_rate"`      // 16000
	FrameMS    int    `json:"frame_ms"`         // 30 → 每次上行 30ms
	Channels   int    `json:"channels"`         // 1
}

// PlayStartPayload 启动播放。PrebufferMS 为起播预缓冲（越小越快出声，但弱网易断）。
type PlayStartPayload struct {
	SampleRate  int `json:"sample_rate"` // 24000
	PrebufferMS int `json:"prebuffer_ms"`
	Channels    int `json:"channels"`
}

// PlayStopPayload 停止播放。Flush=true 表示立即丢弃缓冲（打断）。
type PlayStopPayload struct {
	Flush bool `json:"flush"`
}

// TonePayload 让音箱本地合成一个提示音（服务端不必下发音频）。
type TonePayload struct {
	// Freq2Hz/Duration2MS 非 0 时，在同一次播放里连播第二个音（"叮咚"这种双音）。
	// 分成两次命令会被播放器重启切断，听起来只剩后一个。
	Freq2Hz     int `json:"freq2_hz,omitempty"`
	Duration2MS int `json:"duration2_ms,omitempty"`
	FreqHz      int `json:"freq_hz"`
	DurationMS  int `json:"duration_ms"`
	Gate        int `json:"gate_ms"`   // 每段响/停的间隔，0 表示连续
	Amplitude   int `json:"amplitude"` // 0-32767
}

// VolumePayload 设置音量 0-100。
type VolumePayload struct {
	Level int `json:"level"`
}

// ShellPayload 限本机执行脚本（仅后台使用，不暴露给 LLM）。
type ShellPayload struct {
	Script    string `json:"script"`
	TimeoutMS int    `json:"timeout_ms"`
}

// ShellResult 是 shell.run 的返回。
type ShellResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// SayPayload 让音箱用自己的 TTS 念一段文字。
//
// 为什么要这条路：本地 VITS 只能用 8kHz 模型（16k/44k 的 RTF >3，跑不动），
// 8kHz 又加速到 1.75 倍，听起来就是"吐字不清"。小爱原生 TTS 是云端合成，
// 清晰度和自然度都是另一个档次，代价是依赖网络、语速不可调。
type SayPayload struct {
	Text string `json:"text"`
	// NoDone 表示这句话是"收尾语"（比如退出应答），
	// 播完之后不要加"说完了"的提示音——用户已经听到告别语了，
	// 再加一声会让这个音失去区分度。
	NoDone bool `json:"no_done,omitempty"`
	// NoMute 表示播这句话时**不要闭麦**。
	// 唤醒应答（"诶"）只有一两个音节，为了它闭麦会吞掉用户紧接着说的
	// 第一个词；不闭麦顶多把"诶"自己收进去，服务端会剥掉开头的语气词。
	NoMute bool `json:"no_mute,omitempty"`
}

// SayResult 是 say 的返回（入队即返回，不等播完）。
type SayResult struct {
	Queued bool `json:"queued"`
}

// KWSSyncPayload 把服务端的唤醒词清单推给设备。
//
// 为什么由服务端下发：唤醒词在后台配（跟着助手档案走），
// 而文件在音箱上。不同步就只能登设备改文件，改完还容易和档案对不上。
type KWSSyncPayload struct {
	Path      string   `json:"path,omitempty"` // 落盘路径，空则用客户端默认
	Words     []string `json:"words"`          // 去重后的唤醒词
	Rev       string   `json:"rev"`            // 清单版本（内容哈希），设备据此跳过重复写
	Score     float32  `json:"score,omitempty"`
	Threshold float32  `json:"threshold,omitempty"`
}

// KWSSyncResult 是 kws.sync 的返回。
type KWSSyncResult struct {
	OK      bool   `json:"ok"`
	Path    string `json:"path"`
	Count   int    `json:"count"`
	Rev     string `json:"rev"`
	Skip    bool   `json:"skip,omitempty"` // 版本一致，未重写
	Bytes   int    `json:"bytes"`
	Message string `json:"message,omitempty"`
}

// DeviceInfo 是 device.info 的返回。
type DeviceInfo struct {
	Model      string  `json:"model"`
	Firmware   string  `json:"firmware"`
	UptimeSec  int64   `json:"uptime_sec"`
	DiskFreeMB int64   `json:"disk_free_mb"`
	MemFreeMB  int64   `json:"mem_free_mb"`
	Load1      float64 `json:"load1"`
	Volume     int     `json:"volume"`
}

// AudioLevel 是 event.audio 的负载（每 100ms 一次，供后台画波形）。
type AudioLevel struct {
	Level int     `json:"level"` // 0-100 的近似响度
	RMS   float64 `json:"rms"`   // 原始均方根
	Peak  int     `json:"peak"`
}

// PlayingState 是 event.playing 的负载。
type PlayingState struct {
	State string `json:"state"` // playing|paused|idle
}

// KWSState 是 event.kws 的负载。
type KWSState struct {
	Keyword string `json:"keyword,omitempty"`
	Started bool   `json:"started,omitempty"`
}

// InstructionEvent 是 event.instruction 的负载。
// Line 是小爱原生 instruction.log 的一行（原始 JSON 字符串）。
type InstructionEvent struct {
	Line string `json:"line"`
}

// FatalEvent 客户端遇到不可恢复错误时上报。
type FatalEvent struct {
	Reason string `json:"reason"`
}
