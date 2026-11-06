package audio

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func s32Sample(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}

func TestS32ToS16ShiftIsEightNotSixteen(t *testing.T) {
	cases := []struct {
		name string
		in   int32
		want int16
	}{
		{"静音", 0, 0},
		{"最低位 256 → 1", 0x0000_0100, 1},
		{"-256 → -1", -0x0000_0100, -1},
		{"典型语音量级", 0x0010_0000, 0x1000}, // >>8 = 0x1000
		{"正向上溢被饱和", 0x7FFF_FF00, 32767}, // >>8 = 0x7FFFFF
		{"负向饱和到下限", -0x7FFF_FF00, -32768},
		{"INT32 最小值", -2147483648, -32768},
	}
	for _, c := range cases {
		dst := make([]byte, 2)
		s32ToS16(s32Sample(c.in), dst, 1)
		got := int16(binary.LittleEndian.Uint16(dst))
		if got != c.want {
			t.Errorf("%s: 输入 %#x 期望 %d 实际 %d", c.name, uint32(c.in), c.want, got)
		}
	}
}

func TestS32ToS16WouldBeSilentIfShiftSixteen(t *testing.T) {
	// 回归测试：如果误用 >>16，典型语音会被压成 0 或极小值
	in := int32(0x0010_0000) // 1048576
	dst := make([]byte, 2)
	s32ToS16(s32Sample(in), dst, 1)
	got := int16(binary.LittleEndian.Uint16(dst))
	if got == 0 {
		t.Fatal("右手边被移空了：说明位移量写成了 16")
	}
	if got != 4096 {
		t.Fatalf("期望 4096，实际 %d", got)
	}
}

func TestCaptureArgsUseS32AndDevice(t *testing.T) {
	c := DefaultCapture()
	args := strings.Join(c.Args(), " ")

	for _, want := range []string{"-f S32_LE", "-D noop", "-r 16000", "-c 1"} {
		if !strings.Contains(args, want) {
			t.Errorf("arecord 参数缺少 %q: %s", want, args)
		}
	}
	// 采集必须带 -D；播放才不加
	if strings.Contains(args, "S16_LE") {
		t.Error("采集不能用 S16_LE，会被驱动上采导致量纲错误")
	}
}

func TestFrameBytes(t *testing.T) {
	c := DefaultCapture() // 16k / 30ms / mono
	if got := c.frameBytes(); got != 960 {
		t.Fatalf("30ms@16k 单声道应为 960 字节，实际 %d", got)
	}
	c.FrameMS = 60
	if got := c.frameBytes(); got != 1920 {
		t.Fatalf("60ms 应为 1920 字节，实际 %d", got)
	}
}

func TestCaptureNormalized(t *testing.T) {
	var zero CaptureConfig
	got := zero.normalized()
	if got.Device != "noop" || got.SampleRate != 16000 || got.FrameMS != 30 || got.Channels != 1 {
		t.Fatalf("默认值填充不正确: %+v", got)
	}
}

func TestLevelComputation(t *testing.T) {
	// 满量程正弦的 RMS ≈ 0.707，这里用恒定值近似
	b := s32Sample(0x7F00_0000)
	sumSq, count, peak := accumulate(0, 0, 0, b)
	lv := buildLevel(sumSq, count, peak)
	if lv.Level < 90 {
		t.Fatalf("接近满量程时 level 应接近 100，实际 %d", lv.Level)
	}
	if lv.Peak == 0 {
		t.Fatal("peak 不应为 0")
	}

	if zero := buildLevel(0, 0, 0); zero.Level != 0 {
		t.Fatalf("无采样时应为 0，实际 %d", zero.Level)
	}
}

// chunkReader 按给定序列返回数据，模拟 arecord 不规则的分块（真机上是 3200 字节）。
type chunkReader struct {
	chunks [][]byte
	i      int
	off    int // 当前块内偏移：一次 Read 装不下时要留到下次
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for c.i < len(c.chunks) && c.off >= len(c.chunks[c.i]) {
		c.i++
		c.off = 0
	}
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i][c.off:])
	c.off += n
	return n, nil
}

// 真机上 arecord 每次给 3200 字节，不是 1920（S32 帧长）的整数倍。
// 曾经因为"判断用 S16 帧长、推进用 S32 帧长"而 slice 越界 panic。
func TestReadLoopHandlesChunkNotMultipleOfFrame(t *testing.T) {
	cfg := DefaultCapture() // 16k / 30ms / 单声道 → S16 帧 960B，S32 帧 1920B
	r := NewRecorder(cfg)

	// 造 5 帧的数据 = 5*1920 = 9600 字节 S32
	const total = 5 * 1920
	src := make([]byte, total)
	for i := 0; i < total; i += 4 {
		// 每个采样填 0x00000100 << 8 的形态，降位后应等于 0x0100
		binary.LittleEndian.PutUint32(src[i:], uint32(i%32768)<<8)
	}

	// 按 3200 / 1320 / 5080 切开，全都不是 1920 的倍数
	rd := &chunkReader{chunks: splitAt(src, 3200, 1320, 5080)}

	var frames int
	var sizes []int
	var firstFrame []byte
	r.readLoop(context.Background(), rd, func(b []byte) {
		if frames == 0 {
			firstFrame = append([]byte(nil), b...)
		}
		frames++
		sizes = append(sizes, len(b))
	}, func(Level) {})

	if frames != 5 {
		t.Fatalf("应切出 5 帧，实际 %d（块长不是帧长整数倍时不能丢帧）", frames)
	}
	for i, sz := range sizes {
		if sz != 960 {
			t.Fatalf("第 %d 帧长度应为 960，实际 %d", i, sz)
		}
	}
	// 首帧第一个采样：src[0] = 0<<8 = 0，降位后仍是 0
	if len(firstFrame) != 960 {
		t.Fatalf("首帧长度不对: %d", len(firstFrame))
	}
	if got := binary.LittleEndian.Uint16(firstFrame); got != 0 {
		t.Fatalf("首帧首个采样应为 0，实际 %d", got)
	}
	// 第二个采样：src[4] = (4%32768)<<8 = 1024，降位 >>8 = 4
	if got := binary.LittleEndian.Uint16(firstFrame[2:]); got != 4 {
		t.Fatalf("第二个采样应为 4（S32>>8），实际 %d", got)
	}
}

// 半帧数据要留着等下个 chunk 拼起来，不能丢。
func TestReadLoopJoinsPartialFrames(t *testing.T) {
	r := NewRecorder(DefaultCapture())
	src := make([]byte, 1920*2)                               // 2 帧
	rd := &chunkReader{chunks: splitAt(src, 1000, 920, 1920)} // 前两块拼成 1 帧

	var frames int
	r.readLoop(context.Background(), rd, func([]byte) { frames++ }, func(Level) {})
	if frames != 2 {
		t.Fatalf("跨界数据应拼接成 2 帧，实际 %d", frames)
	}
}

// 0 字节读取不应死循环。
func TestReadLoopEmptyChunkTerminates(t *testing.T) {
	r := NewRecorder(DefaultCapture())
	rd := &chunkReader{chunks: [][]byte{{}, {}}}
	done := make(chan int, 1)
	go func() {
		var n int
		r.readLoop(context.Background(), rd, func([]byte) { n++ }, func(Level) {})
		done <- n
	}()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("空数据不该产生帧，实际 %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop 没有退出")
	}
}

// splitAt 按给定长度切块（总和超过时截断）。
func splitAt(b []byte, sizes ...int) [][]byte {
	var out [][]byte
	off := 0
	for _, n := range sizes {
		if off >= len(b) {
			break
		}
		end := off + n
		if end > len(b) {
			end = len(b)
		}
		out = append(out, b[off:end])
		off = end
	}
	if off < len(b) {
		out = append(out, b[off:])
	}
	return out
}

// arecord 自己退出后，Running() 必须变 false —— 上层看门狗靠它决定要不要重开麦。
// 不清状态的话表现是"永久静音，日志里只有一行停止"，极难排查。
func TestRecorderRunningBecomesFalseAfterProcessExit(t *testing.T) {
	dir := t.TempDir()
	// 假 arecord：吐 4 帧（4×1920 字节）就退出
	script := filepath.Join(dir, "arecord")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nhead -c 7680 /dev/zero\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	r := NewRecorder(DefaultCapture())
	var frames int
	if err := r.Start(context.Background(), func([]byte) { frames++ }, func(Level) {}); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer r.Stop()

	// 10 秒而不是 5 秒：-race 下整个进程慢一倍以上，5 秒会偶发超时，
	// 而这里要验的是"会不会变 false"，不是"多快变 false"。
	deadline := time.Now().Add(10 * time.Second)
	for r.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.Running() {
		t.Fatal("子进程退出后 Running() 必须变 false，否则看门狗不会重开麦")
	}
	if frames != 4 {
		t.Fatalf("应收到 4 帧，实际 %d", frames)
	}
}
