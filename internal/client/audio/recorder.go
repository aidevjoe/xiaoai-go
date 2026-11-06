// Package audio 负责音箱端的采集与播放。
//
// 关键事实（来自对 Rust 版 client 的实测与源码核对）：
//   - 音箱的 PDM 麦克风只能以 S32_LE 采集，请求 16 位也会被驱动"上采"为 32 位
//   - A113 的有效数据在低 24 位，所以 S32→S16 必须右移 8 位（不是 16 位）
//   - 播放用 aplay 且不加 -D（走系统默认设备），采集必须带 -D
package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// CaptureConfig 描述一次录音。
type CaptureConfig struct {
	Device     string // ALSA 设备名，LX06 上是 "noop"
	SampleRate int    // 16000
	FrameMS    int    // 每次上行的帧长（毫秒），30 是实测最优
	Channels   int    // 1
}

// DefaultCapture 返回 LX06 上的实测参数。
func DefaultCapture() CaptureConfig {
	return CaptureConfig{Device: "noop", SampleRate: 16000, FrameMS: 30, Channels: 1}
}

func (c CaptureConfig) normalized() CaptureConfig {
	if c.Device == "" {
		c.Device = "noop"
	}
	if c.SampleRate <= 0 {
		c.SampleRate = 16000
	}
	if c.FrameMS <= 0 {
		c.FrameMS = 30
	}
	if c.Channels <= 0 {
		c.Channels = 1
	}
	return c
}

// frameBytes 返回每帧输出的字节数（S16 立体声以下）。
func (c CaptureConfig) frameBytes() int {
	return c.SampleRate * c.FrameMS / 1000 * 2 * c.Channels
}

// periodFrames 是 arecord 每次读取的帧数（10ms @16k）。
func (c CaptureConfig) periodFrames() int {
	p := c.SampleRate / 100
	if p < 1 {
		p = 1
	}
	return p
}

// bufferFrames 是 ALSA 缓冲（3 个 period）。
func (c CaptureConfig) bufferFrames() int { return c.periodFrames() * 3 }

// Level 是一次音量统计，供后台画波形。
type Level struct {
	RMS   float64
	Peak  int
	Level int // 0-100
}

// Recorder 驱动 arecord 并把音频切成固定长度的帧。
type Recorder struct {
	cfg  CaptureConfig
	seq  uint32
	mu   sync.Mutex
	cmd  *exec.Cmd
	stop context.CancelFunc
	done chan struct{}
}

// NewRecorder 构造录音器。
func NewRecorder(cfg CaptureConfig) *Recorder {
	return &Recorder{cfg: cfg.normalized()}
}

// Args 返回 arecord 的完整参数（抽出来便于测试）。
func (c CaptureConfig) Args() []string {
	return []string{
		"--quiet",
		"-t", "raw",
		"-D", c.Device,
		"-f", "S32_LE", // 始终 32 位采集，再在 Go 侧降位
		"-r", fmt.Sprint(c.SampleRate),
		"-c", fmt.Sprint(c.Channels),
		"--buffer-size", fmt.Sprint(c.bufferFrames()),
		"--period-size", fmt.Sprint(c.periodFrames()),
	}
}

// Start 启动采集。每凑满一帧调用 onFrame（payload 为 S16LE），
// 每约 100ms 调用一次 onLevel。
func (r *Recorder) Start(ctx context.Context, onFrame func(payload []byte), onLevel func(Level)) error {
	r.mu.Lock()
	if r.cmd != nil {
		r.mu.Unlock()
		return errors.New("audio: 录音已在进行中")
	}
	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, "arecord", r.cfg.Args()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		r.mu.Unlock()
		return fmt.Errorf("audio: 获取 arecord 输出失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		r.mu.Unlock()
		return fmt.Errorf("audio: 启动 arecord 失败: %w", err)
	}
	r.cmd = cmd
	r.stop = cancel
	done := make(chan struct{})
	r.done = done
	r.mu.Unlock()

	go func() {
		defer close(done)
		r.readLoop(runCtx, stdout, onFrame, onLevel)
		err := cmd.Wait()

		// 清掉 cmd 很重要：Running() 就是看它。
		// 不清的话 arecord 已经死了，上层还以为在录音，
		// 表现为"音频永久静音、日志里什么都看不出来"。
		r.mu.Lock()
		if r.cmd == cmd {
			r.cmd = nil
			r.stop = nil
		}
		r.mu.Unlock()

		if err != nil && runCtx.Err() == nil {
			// arecord 自己退出了（设备被占用/参数错），交给上层决定是否重启
			onLevel(Level{})
		}
	}()
	return nil
}

// readLoop 读取 S32_LE 字节流，降位后按帧长切帧。
//
// 注意两个长度别再搞混（真机上就是这里踩坑）：
//   - frameS16 是一帧**输出**的字节数（960）
//   - frameS32 是一帧**输入**的字节数（1920）
//
// arecord 每次给的量不是帧长的整数倍（典型 3200 字节），所以判断和推进
// 必须都用 frameS32，否则会 slice 越界 panic。
func (r *Recorder) readLoop(ctx context.Context, rd io.Reader, onFrame func([]byte), onLevel func(Level)) {
	frameS16 := r.cfg.frameBytes()
	frameS32 := frameS16 * 2
	periodS32 := r.cfg.periodFrames() * 4 * r.cfg.Channels

	raw := make([]byte, periodS32) // arecord 每次给的量
	// 容量留一个 period + 一帧，够拼接跨界的数据
	pending := make([]byte, 0, periodS32+frameS32)
	s16 := make([]byte, frameS16) // 复用的输出缓冲（每帧拷贝一份再回调）

	lastLevel := time.Now()
	var sumSq float64
	var sampleCount, peak int

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := rd.Read(raw)
		if n > 0 {
			pending = append(pending, raw[:n]...)
			// 统计音量（用降位后的采样）
			sumSq, sampleCount, peak = accumulate(sumSq, sampleCount, peak, raw[:n])

			consumed := 0
			for len(pending)-consumed >= frameS32 {
				// S32 → S16：每 4 字节取中间两个字节（等价位运算 >>8）
				s32ToS16(pending[consumed:consumed+frameS32], s16, r.cfg.Channels)
				buf := make([]byte, frameS16)
				copy(buf, s16)
				onFrame(buf)
				consumed += frameS32
			}
			// 把没用完的尾巴挪到开头，保持 backing array 不增长
			if consumed > 0 {
				pending = pending[:copy(pending, pending[consumed:])]
			}

			if time.Since(lastLevel) >= 100*time.Millisecond {
				onLevel(buildLevel(sumSq, sampleCount, peak))
				lastLevel = time.Now()
				sumSq, sampleCount, peak = 0, 0, 0
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				// 读失败，直接退出，上层会重启
				return
			}
			return
		}
	}
}

// accumulate 累计 RMS 统计。输入是 S32LE 字节。
func accumulate(sumSq float64, count, peak int, b []byte) (float64, int, int) {
	for i := 0; i+3 < len(b); i += 4 {
		v := int32(binary.LittleEndian.Uint32(b[i:]))
		v >>= 8 // 与 s32ToS16 保持一致的量纲
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
		f := float64(v)
		sumSq += f * f
		count++
	}
	return sumSq, count, peak
}

func buildLevel(sumSq float64, count, peak int) Level {
	if count == 0 {
		return Level{}
	}
	rms := sqrt(sumSq / float64(count))
	lv := int(rms / 32768 * 100 * 3) // 语音通常只占满量程的 1/3 左右
	if lv > 100 {
		lv = 100
	}
	return Level{RMS: rms, Peak: peak, Level: lv}
}

// s32ToS16 把 S32_LE 降为 S16_LE。
// 音箱 PDM 的有效数据在低 24 位，因此右移 8 位并饱和到 int16。
// 注意：不是右移 16 位——那样会得到一片静音。
func s32ToS16(src []byte, dst []byte, channels int) {
	frames := len(src) / 4
	for i := 0; i < frames; i++ {
		v := int32(binary.LittleEndian.Uint32(src[i*4:]))
		v >>= 8
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(v)))
	}
	_ = channels
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// 不引 math，减少依赖体积；牛顿迭代 3 次足够画波形
	z := x
	for i := 0; i < 8; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// Stop 停止采集并等待退出。
func (r *Recorder) Stop() {
	r.mu.Lock()
	cmd, cancel, done := r.cmd, r.stop, r.done
	r.cmd, r.stop, r.done = nil, nil, nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Running 报告是否在录音。
func (r *Recorder) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil
}

var _ = atomic.AddUint32 // 预留：seq 由上层管理
