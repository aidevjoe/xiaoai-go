// Package wsclient 是音箱端到服务端的 WebSocket 客户端。
//
// 与旧实现的差异：
//   - 自带最小 WebSocket 实现（ws.go），握手不走 net/http，二进制小一半
//   - 打开 TCP_NODELAY（关掉 Nagle 合包），避免 30ms 一帧的音频被攒到 40ms 才发
//   - 音频走二进制裸 PCM，不再塞 JSON 数字数组
//   - 写操作单一互斥，读循环只读，避免并发写导致的帧交错
package wsclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"xiaoai-go/internal/proto"
)

// Client 是一条到服务端的连接。
type Client struct {
	conn    *conn
	wmu     sync.Mutex
	scratch []byte
	seq     atomic.Uint32
	closed  atomic.Bool
	onPing  func()
}

// DialOptions 控制建连行为。
type DialOptions struct {
	Token   string        // Bearer token，空则不鉴权
	Timeout time.Duration // 握手超时，默认 5s
	UA      string        // User-Agent，空则用默认值
}

// Dial 建立连接。
func Dial(ctx context.Context, rawURL string, opts DialOptions) (*Client, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	u, err := parseTarget(rawURL)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nc, err := rawConnect(dialCtx, u, timeout)
	if err != nil {
		return nil, fmt.Errorf("wsclient: 连接 %s 失败: %w", rawURL, err)
	}

	ua := opts.UA
	if ua == "" {
		ua = "xiaoai-go-client"
	}
	header := map[string]string{
		"User-Agent": ua,
	}
	if opts.Token != "" {
		header["Authorization"] = "Bearer " + opts.Token
	}

	br := bufio.NewReaderSize(nc, 8192)
	if err := handshake(nc, br, u, header); err != nil {
		_ = nc.Close()
		return nil, err
	}

	return &Client{
		conn:    &conn{nc: nc, br: br, wbuf: make([]byte, 0, 256)},
		scratch: make([]byte, 0, proto.HeaderSize+4096),
	}, nil
}

// Send 发送控制帧。
func (c *Client) Send(ctx context.Context, e *proto.Envelope) error {
	if c.closed.Load() {
		return ErrClosed
	}
	b, err := e.Marshal()
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conn.writeMessage(ctx, MessageText, b)
}

// SendFrame 发送音频帧。
func (c *Client) SendFrame(ctx context.Context, f *proto.Frame) error {
	if c.closed.Load() {
		return ErrClosed
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.scratch, _ = f.AppendTo(c.scratch[:0])
	return c.conn.writeMessage(ctx, MessageBinary, c.scratch)
}

// NextSeq 返回下一个音频帧序号。
func (c *Client) NextSeq() uint32 { return c.seq.Add(1) }

// Recv 读一条消息，返回控制帧或音频帧（二选一）。
//
// 注意：ctx 只用于给本次读设超时上限；想中断阻塞中的读请调用 Close。
func (c *Client) Recv(ctx context.Context) (*proto.Envelope, *proto.Frame, error) {
	for {
		typ, data, err := c.conn.readMessage(ctx)
		if err != nil {
			return nil, nil, err
		}
		switch typ {
		case MessageText:
			e, err := proto.ParseEnvelope(data)
			if err != nil {
				continue // 忽略脏帧，不因此断连
			}
			if e.Type == proto.TypePing {
				_ = c.Send(ctx, &proto.Envelope{V: proto.Version, Type: proto.TypePong, TS: e.TS})
				if c.onPing != nil {
					c.onPing()
				}
				continue
			}
			return e, nil, nil
		case MessageBinary:
			f, err := proto.ParseFrame(data)
			if err != nil {
				continue
			}
			return nil, f, nil
		}
	}
}

// Ping 主动发一个 ping 控制帧（不用 WS 层 ping，便于端到端测 RTT）。
func (c *Client) Ping(ctx context.Context) error {
	return c.Send(ctx, &proto.Envelope{V: proto.Version, Type: proto.TypePing, TS: proto.NowMS()})
}

// Close 关闭连接，并唤醒可能阻塞在 Recv 上的协程。
func (c *Client) Close(reason string) {
	if c.closed.Swap(true) {
		return
	}
	_ = c.conn.close(1000, reason)
}

// Closed 报告连接是否已关闭。
func (c *Client) Closed() bool { return c.closed.Load() }

// LocalAddr 返回本地地址（日志排查用）。
func (c *Client) LocalAddr() net.Addr { return c.conn.nc.LocalAddr() }

// IsNormalClose 判断读循环退出是否属于正常关闭。
func (c *Client) IsNormalClose(err error) bool { return IsNormalClose(err) }

var _ = errors.Is
