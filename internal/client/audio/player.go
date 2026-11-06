package audio

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// PlayConfig 描述一次播放。
type PlayConfig struct {
	SampleRate  int // 24000（小爱扬声器在 24k 下最稳）
	Channels    int
	PrebufferMS int // 起播前先攒多少毫秒，避免一上来就 underrun
}

// DefaultPlay 返回实测参数。
func DefaultPlay() PlayConfig {
	return PlayConfig{SampleRate: 24000, Channels: 1, PrebufferMS: 120}
}

func (c PlayConfig) normalized() PlayConfig {
	if c.SampleRate <= 0 {
		c.SampleRate = 24000
	}
	if c.Channels <= 0 {
		c.Channels = 1
	}
	if c.PrebufferMS < 0 {
		c.PrebufferMS = 0
	}
	return c
}

// Args 返回 aplay 参数。
// 注意：播放不带 -D（走系统默认设备），只有采集才带 -D。
func (c PlayConfig) Args() []string {
	return []string{
		"-q",
		"-t", "raw",
		"-f", "S16_LE",
		"-r", fmt.Sprint(c.SampleRate),
		"-c", fmt.Sprint(c.Channels),
	}
}

// prebufferBytes 返回起播前需要攒的字节数。
func (c PlayConfig) prebufferBytes() int {
	return c.SampleRate * c.PrebufferMS / 1000 * 2 * c.Channels
}

// sink 抽象出"往哪写"，便于测试时换成 buffer。
type sink interface {
	io.WriteCloser
}

// Player 是常驻 aplay 的流式播放器。
type Player struct {
	mu     sync.Mutex
	cfg    PlayConfig
	w      sink
	cmd    *exec.Cmd
	buf    []byte // 预缓冲
	closed bool

	// 可注入，测试时替换
	openSink func(PlayConfig) (sink, *exec.Cmd, error)
}

// NewPlayer 构造播放器。
func NewPlayer() *Player {
	return &Player{openSink: openAplay}
}

func openAplay(cfg PlayConfig) (sink, *exec.Cmd, error) {
	cmd := exec.Command("aplay", cfg.Args()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("audio: 获取 aplay stdin 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("audio: 启动 aplay 失败: %w", err)
	}
	return stdin, cmd, nil
}

// Start 开始一次播放会话，若有旧会话先停掉（等价于"重新开始"）。
func (p *Player) Start(cfg PlayConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.w != nil || p.cmd != nil {
		p.stopLocked(true)
	}
	p.cfg = cfg.normalized()
	p.buf = p.buf[:0]
	p.closed = false
	// 预缓冲为 0 时立即起 aplay，其余情况等 Write 攒够再起
	if p.cfg.prebufferBytes() == 0 {
		return p.openLocked()
	}
	return nil
}

func (p *Player) openLocked() error {
	w, cmd, err := p.openSink(p.cfg)
	if err != nil {
		return err
	}
	p.w = w
	p.cmd = cmd
	return nil
}

// Write 追加 PCM。攒够预缓冲后自动起播，之后是阻塞写（不做超时取消，
// 避免半写导致音频流断裂——这是旧实现的一个缺陷）。
func (p *Player) Write(pcm []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return errors.New("audio: 播放器已关闭")
	}
	if p.w == nil {
		p.buf = append(p.buf, pcm...)
		if len(p.buf) < p.cfg.prebufferBytes() {
			return nil
		}
		if err := p.openLocked(); err != nil {
			return err
		}
		if _, err := p.w.Write(p.buf); err != nil {
			return err
		}
		p.buf = p.buf[:0]
		return nil
	}
	_, err := p.w.Write(pcm)
	return err
}

// Stop 停止播放。flush=true 表示立即丢弃缓冲（打断用）。
func (p *Player) Stop(flush bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked(flush)
}

func (p *Player) stopLocked(flush bool) {
	if !flush && len(p.buf) > 0 && p.w != nil {
		// 温和停止：把剩余缓冲写完再关
		_, _ = p.w.Write(p.buf)
	}
	p.buf = p.buf[:0]
	if p.w != nil {
		_ = p.w.Close()
		p.w = nil
	}
	if p.cmd != nil {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(termSignal())
			done := make(chan struct{})
			cmd := p.cmd
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(100 * time.Millisecond):
				_ = p.cmd.Process.Kill()
				<-done
			}
		}
		p.cmd = nil
	}
	p.closed = true
}

// Running 报告是否正在播放。
func (p *Player) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.w != nil || len(p.buf) > 0
}

// Buffered 返回预缓冲中尚未播放的字节数（供后台观测起播延迟）。
func (p *Player) Buffered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}
