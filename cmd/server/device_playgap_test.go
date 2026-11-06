package main

import (
	"testing"
	"time"
)

// 回归：合成下一句要 1~2 秒，但上一句音频往往还在播。
// 旧逻辑用"距上次下发 > 700ms"判断播完了，会在这时重开 aplay，
// 把已下发未播完的尾部丢掉 —— 听感是"半句突然断掉、直接接后文"。
func TestPlaySession_NotRestartedWhilePreviousAudioIsPlaying(t *testing.T) {
	const rate = 22050
	tgt := &deviceTarget{}
	t0 := time.Now()

	if !tgt.shouldStart(rate, t0) {
		t.Fatal("第一次下发应开新会话")
	}
	tgt.started = true
	tgt.lastRate = rate
	tgt.playEndsAt = t0.Add(playPrebuffer)
	// 第一段 2 秒音频（实测 13 字句子约 2.1s @22050Hz）
	tgt.notePCM(rate, rate*2*2, t0)

	// 下一句合成耗时 1.5s，远高于旧的 700ms 阈值
	at := t0.Add(1500 * time.Millisecond)
	if tgt.shouldStart(rate, at) {
		t.Fatalf("上一段还有 %.0fms 才播完，不该重开播放会话",
			float64(tgt.playEndsAt.Sub(at).Microseconds())/1000)
	}
	// 继续拼接第二段：播完时刻应往后顺延，而不是从 now 重新起算
	before := tgt.playEndsAt
	tgt.notePCM(rate, rate*2, at) // 第二段 1 秒
	if !tgt.playEndsAt.After(before) {
		t.Fatal("连续下发时播放队列应该累加，不能被重置")
	}
}

// 一轮结束、音频确实播完之后，下一轮才允许重开会话。
func TestPlaySession_RestartsOnNewTurnAndRateChange(t *testing.T) {
	const rate = 22050
	tgt := &deviceTarget{started: true, lastRate: rate}
	t0 := time.Now()
	tgt.notePCM(rate, rate*2*2, t0)

	if !tgt.shouldStart(rate, t0.Add(4*time.Second)) {
		t.Fatal("上一轮音频早已播完，新一轮应重开会话")
	}
	if !tgt.shouldStart(16000, t0) {
		t.Fatal("采样率变了必须重开 aplay")
	}
	if !(&deviceTarget{}).shouldStart(rate, t0) {
		t.Fatal("全新的 target 必须开会话")
	}
}
