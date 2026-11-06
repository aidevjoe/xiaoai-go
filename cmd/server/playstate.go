package main

// 设备播放状态跟踪：播报期间闭麦。
//
// 为什么必须做：服务端把 TTS 推给音箱后，PlayPCM 只是把帧写出去，
// 音箱的 aplay 还在缓冲里播几百毫秒。这段时间麦克风仍在采集，
// 于是"自己说的话"又被识别成用户输入——免唤醒模式下直接变成自我对话
// （实测出现过「问：主我没听清，你想让我做什么。」）。
//
// 客户端每 300ms 用 mphelper mute_stat 轮询播放状态并上报变化，
// 服务端据此在播放期间丢弃上行音频；播完再留一小段尾巴。

import "time"

// playTail 是播放结束后的额外闭麦时间，盖住 aplay 缓冲与房间混响。
const playTail = 350 * time.Millisecond

// shortTail 用于"短促声音"（提示音 50ms）之后的闭麦。
// 提示音本来就短，留 700ms 会把用户紧跟着说的头一个字吃掉
// （KWS 唤醒就是这种情况：叮一声之后马上开始提问）。
const shortTail = 200 * time.Millisecond

// shortSound 小于这个时长就按"短应答"处理（"我在""好的"这类），
// 播完只闭麦 shortTail。
//
// 取 1.2 秒：唤醒应答"我在"约 0.9 秒，如果按"说了一句话"闭麦 700ms，
// 正好盖住用户紧跟着说的第一个字——日志里"今天天气怎么样"被识别成
// "天天气怎么样"就是这个原因。
const shortSound = 1200 * time.Millisecond

// playStuck 是兜底：状态卡在 playing 超过这么久就强制解除闭麦，
// 否则客户端上报丢一次事件，麦克风就永久哑了。
const playStuck = 30 * time.Second

type playInfo struct {
	playing bool
	paused  bool      // 处于"暂停"（声音随时可能继续）
	since   time.Time // 进入 playing 的时刻
	muteTil time.Time // 播完后继续闭麦到这个时间
}

// setPlaying 更新设备的播放状态。
func (s *server) setPlaying(id, state string) {
	now := time.Now()
	s.playMu.Lock()
	defer s.playMu.Unlock()
	if s.play == nil {
		s.play = make(map[string]playInfo)
	}
	pi := s.play[id]
	switch state {
	case "playing":
		if !pi.playing {
			pi.since = now
		}
		pi.playing = true
		pi.paused = false
	case "paused":
		// 暂停时用户在听，但声音随时会继续，按"仍在播"处理更安全。
		// 但必须限时：状态一旦卡在 paused（客户端映射错、或播放器异常退出），
		// 这里会永久闭麦，表现为"喊唤醒词完全没反应"。
		if !pi.playing {
			pi.since = now
		}
		pi.playing = true
		pi.paused = true
	default: // idle
		if pi.playing {
			tail := playTail
			// 短促的声音（提示音）不需要长尾巴，否则会吃掉紧跟着的提问开头
			if !pi.since.IsZero() && now.Sub(pi.since) < shortSound {
				tail = shortTail
			}
			pi.muteTil = now.Add(tail)
		}
		pi.playing = false
		pi.paused = false
	}
	s.play[id] = pi
}

// pauseMuteMax 是"暂停"状态下最长闭麦时间。
// 超过就当作没在播：宁可偶尔收进一点自己的声音，也不能永久闭麦。
const pauseMuteMax = 6 * time.Second

// deviceMuted 判断该设备当前是否应该闭麦。
func (s *server) deviceMuted(id string) bool {
	now := time.Now()
	s.playMu.Lock()
	defer s.playMu.Unlock()
	pi, ok := s.play[id]
	if !ok {
		return false
	}
	if pi.playing {
		stuck := playStuck
		if pi.paused {
			stuck = pauseMuteMax // 暂停最多闭 6 秒，多了就是卡住
		}
		if !pi.since.IsZero() && now.Sub(pi.since) > stuck {
			// 状态卡住：放行，别把麦克风一直关着
			pi.playing = false
			pi.paused = false
			pi.muteTil = now
			s.play[id] = pi
			return false
		}
		return true
	}
	return now.Before(pi.muteTil)
}
