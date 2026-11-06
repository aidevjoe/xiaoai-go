package main

import (
	"testing"
	"time"
)

// 状态卡在 "paused" 时必须限时放行。
//
// 事故现场：客户端用 mphelper mute_stat 判断播放状态，而这个命令在空闲和
// 播放时都返回 2，被映射成 paused；服务端又把 paused 当作"正在播"，
// 于是每轮回答之后麦克风被关 30 秒——那段时间喊唤醒词完全没反应。
func TestPausedMuteIsCapped(t *testing.T) {
	s := &server{}
	s.setPlaying("dev", "paused")
	if !s.deviceMuted("dev") {
		t.Fatal("刚进入暂停时应闭麦（声音随时会继续）")
	}

	// 把进入时刻拨回到 7 秒前：超过 pauseMuteMax 就该放行
	s.playMu.Lock()
	pi := s.play["dev"]
	pi.since = time.Now().Add(-7 * time.Second)
	s.play["dev"] = pi
	s.playMu.Unlock()

	if s.deviceMuted("dev") {
		t.Fatal("暂停超过 6 秒还闭麦，等于麦克风被永久关掉")
	}
}

// 正常播放仍然按 playStuck（30 秒）兜底，不能因为修 paused 而变短。
func TestPlayingMuteUsesLongCap(t *testing.T) {
	s := &server{}
	s.setPlaying("dev2", "playing")
	s.playMu.Lock()
	pi := s.play["dev2"]
	pi.since = time.Now().Add(-10 * time.Second)
	s.play["dev2"] = pi
	s.playMu.Unlock()
	if !s.deviceMuted("dev2") {
		t.Fatal("播放 10 秒不该被放行（长回答可能要念 20 秒）")
	}
}
