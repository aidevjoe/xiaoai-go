package audio

import (
	"bytes"
	"encoding/binary"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// fakeSink 记录写入内容，替代真实 aplay。
type fakeSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (f *fakeSink) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *fakeSink) Close() error { return nil }

func (f *fakeSink) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Len()
}

func TestPrebufferDelaysPlayback(t *testing.T) {
	cfg := PlayConfig{SampleRate: 24000, Channels: 1, PrebufferMS: 120} // 5760 字节
	if got := cfg.prebufferBytes(); got != 5760 {
		t.Fatalf("预缓冲字节数应为 5760，实际 %d", got)
	}

	p := NewPlayer()
	sk := &fakeSink{}
	opened := 0
	p.openSink = func(PlayConfig) (sink, *exec.Cmd, error) {
		opened++
		return sk, nil, nil
	}

	if err := p.Start(cfg); err != nil {
		t.Fatal(err)
	}
	if opened != 0 {
		t.Fatal("预缓冲未满时不应启动 aplay")
	}

	if err := p.Write(make([]byte, 3000)); err != nil {
		t.Fatal(err)
	}
	if opened != 0 || sk.Len() != 0 {
		t.Fatalf("预缓冲不足却已起播: opened=%d written=%d", opened, sk.Len())
	}
	if p.Buffered() != 3000 {
		t.Fatalf("缓冲应为 3000，实际 %d", p.Buffered())
	}

	// 再写 3000，累计 6000 ≥ 5760，触发起播并把缓冲一次写出
	if err := p.Write(make([]byte, 3000)); err != nil {
		t.Fatal(err)
	}
	if opened != 1 {
		t.Fatalf("应启动一次 aplay，实际 %d", opened)
	}
	if sk.Len() != 6000 {
		t.Fatalf("起播时应写出全部缓冲 6000 字节，实际 %d", sk.Len())
	}
	if p.Buffered() != 0 {
		t.Fatalf("起播后缓冲应清空，实际 %d", p.Buffered())
	}

	// 之后的写入直接透传
	if err := p.Write(make([]byte, 480)); err != nil {
		t.Fatal(err)
	}
	if sk.Len() != 6480 {
		t.Fatalf("后续写入应直通，累计应为 6480，实际 %d", sk.Len())
	}
}

func TestPlayArgsHaveNoDeviceFlag(t *testing.T) {
	joined := strings.Join(DefaultPlay().Args(), " ")
	if !strings.Contains(joined, "S16_LE") {
		t.Fatalf("播放应使用 S16_LE: %s", joined)
	}
	// aplay 不加 -D（走系统默认设备）；采集才带 -D
	if strings.Contains(joined, "-D") {
		t.Fatalf("aplay 不应带 -D: %s", joined)
	}
	if !strings.Contains(joined, "-r 24000") {
		t.Fatalf("默认采样率应为 24000: %s", joined)
	}
}

func TestToneLengthAndAmplitude(t *testing.T) {
	const rate, dur, amp = 24000, 100, 8000
	pcm := TonePCM(rate, 880, dur, 0, amp)

	wantLen := rate * dur / 1000 * 2
	if len(pcm) != wantLen {
		t.Fatalf("提示音长度应为 %d 字节，实际 %d", wantLen, len(pcm))
	}

	maxAmp := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if v < 0 {
			v = -v
		}
		if v > maxAmp {
			maxAmp = v
		}
	}
	if maxAmp > amp {
		t.Fatalf("峰值 %d 超过设定振幅 %d", maxAmp, amp)
	}
	if maxAmp < amp/2 {
		t.Fatalf("峰值 %d 太低，提示音可能听不见", maxAmp)
	}

	// 首尾接近静音（5ms 淡入淡出），避免爆音
	first := int16(binary.LittleEndian.Uint16(pcm[0:]))
	last := int16(binary.LittleEndian.Uint16(pcm[len(pcm)-2:]))
	if first > 200 || first < -200 {
		t.Fatalf("起始样本 %d 未淡入", first)
	}
	if last > 200 || last < -200 {
		t.Fatalf("结束样本 %d 未淡出", last)
	}
}

func TestToneGateProducesSilence(t *testing.T) {
	// gateMS>0 时应出现成段静音（用于"叮—咚"双音）
	pcm := TonePCM(24000, 880, 200, 50, 8000)
	zeros := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		if binary.LittleEndian.Uint16(pcm[i:]) == 0 {
			zeros++
		}
	}
	if zeros < 1000 {
		t.Fatalf("分段提示音静音样本过少: %d", zeros)
	}
}

func TestBeepHelpers(t *testing.T) {
	cases := map[string][]byte{
		"done":  BeepDone(24000),
		"wake":  BeepWake(24000),
		"error": BeepError(24000),
	}
	for name, pcm := range cases {
		if len(pcm) == 0 {
			t.Fatalf("%s 提示音为空", name)
		}
		if len(pcm)%2 != 0 {
			t.Fatalf("%s 提示音字节数应为偶数", name)
		}
	}
}
