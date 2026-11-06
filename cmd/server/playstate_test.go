package main

import (
	"testing"
	"time"
)

// 播报期间必须闭麦，播完还要留一小段尾巴盖住 aplay 缓冲与混响。
func TestDeviceMutedDuringAndAfterPlayback(t *testing.T) {
	s := &server{}
	if s.deviceMuted("d1") {
		t.Fatal("从没播过的设备不该闭麦")
	}

	s.setPlaying("d1", "playing")
	if !s.deviceMuted("d1") {
		t.Fatal("播放中必须闭麦，否则会听见自己")
	}

	s.setPlaying("d1", "idle")
	if !s.deviceMuted("d1") {
		t.Fatal("播完后的尾巴也要闭麦")
	}

	// 把尾巴拨到过去，模拟时间流逝
	s.playMu.Lock()
	pi := s.play["d1"]
	pi.muteTil = time.Now().Add(-time.Millisecond)
	s.play["d1"] = pi
	s.playMu.Unlock()
	if s.deviceMuted("d1") {
		t.Fatal("尾巴结束后应该恢复收音")
	}
}

// 暂停按"仍在播"处理：用户只是暂停，声音随时会继续。
func TestDeviceMutedWhenPaused(t *testing.T) {
	s := &server{}
	s.setPlaying("d1", "paused")
	if !s.deviceMuted("d1") {
		t.Fatal("暂停时应继续闭麦")
	}
}

// 状态卡在 playing 不能把麦克风永久关掉。
func TestDeviceMutedUnsticks(t *testing.T) {
	s := &server{}
	s.setPlaying("d1", "playing")
	s.playMu.Lock()
	pi := s.play["d1"]
	pi.since = time.Now().Add(-2 * playStuck)
	s.play["d1"] = pi
	s.playMu.Unlock()

	if s.deviceMuted("d1") {
		t.Fatal("playing 卡住超过阈值后应强制解除闭麦")
	}
}

// 提示音只有 50ms，播完不能按"说了一句话"留 700ms 尾巴，
// 否则 KWS 唤醒"叮"一声之后用户紧跟着说的第一个字会被闭麦吃掉。
func TestShortSoundGetsShortTail(t *testing.T) {
	s := &server{}
	s.setPlaying("tone", "playing")
	time.Sleep(50 * time.Millisecond) // 模拟 50ms 提示音
	s.setPlaying("tone", "idle")
	time.Sleep(shortTail + 60*time.Millisecond)
	if s.deviceMuted("tone") {
		t.Fatal("提示音的闭麦尾巴应该很短（150ms 级），不该还在闭麦")
	}

	// 对照：一句话（>=400ms）之后仍应保留长尾巴
	s.setPlaying("speech", "playing")
	time.Sleep(shortSound + 50*time.Millisecond)
	s.setPlaying("speech", "idle")
	time.Sleep(shortTail + 60*time.Millisecond)
	if !s.deviceMuted("speech") {
		t.Fatal("正常语音播完后应保留较长闭麦尾巴（700ms 级）")
	}
}
