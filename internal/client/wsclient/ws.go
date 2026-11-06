// 最小 WebSocket 客户端：只做我们用到的那一小块 RFC 6455。
//
// 为什么不直接用 coder/websocket：它的 Dial 走 net/http，
// 于是把 net/http、crypto/tls、crypto/x509、HTTP/2(hpack)、mime/multipart、
// compress/gzip 全拖进二进制——7MB 里有 4MB 是这套为浏览器场景准备的握手栈。
// 音箱只需要在局域网里连 ws://，握手自己写 40 行就够，且能顺手设好 TCP_NODELAY。
//
// 明确不支持的：permessage-deflate（不协商即可）、代理、重定向、多个子协议。
// wss:// 需要编译时带 -tags tls（见 dial_tls.go），默认构建拒绝 wss 并给出明确提示。
package wsclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MessageType 是 WebSocket 消息类型（只保留文本与二进制）。
type MessageType int

const (
	MessageText   MessageType = 1
	MessageBinary MessageType = 2
)

// 帧操作码
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const (
	// maxMessage 单条消息上限。音频帧最大 ~64KB，1MB 有充足余量。
	maxMessage = 1 << 20
	// maxHeader 握手响应头上限，防止对端灌垃圾。
	maxHeader = 8 << 10

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// ErrClosed 表示连接已被关闭。
var ErrClosed = errors.New("wsclient: 连接已关闭")

// CloseError 表示对端发来的关闭帧。
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("wsclient: 对端关闭 (code=%d)", e.Code)
	}
	return fmt.Sprintf("wsclient: 对端关闭 (code=%d): %s", e.Code, e.Reason)
}

// IsNormalClose 判断错误是否属于"正常关闭"，调用方不该把它当成故障。
func IsNormalClose(err error) bool {
	var ce *CloseError
	if errors.As(err, &ce) {
		return ce.Code == 1000 || ce.Code == 1001
	}
	return errors.Is(err, ErrClosed) || errors.Is(err, io.EOF)
}

// conn 是一条已握手的 WebSocket 连接。
type conn struct {
	nc net.Conn
	br *bufio.Reader

	wmu  sync.Mutex // 写必须串行，否则帧会交错
	wbuf []byte     // 复用的写出缓冲
}

// target 是解析后的连接目标。
type target struct {
	secure bool
	host   string // host:port
	path   string // /path?query
}

// parseTarget 手写解析，避免把 net/url 拉进二进制。
func parseTarget(raw string) (target, error) {
	var u target
	rest := raw
	switch {
	case strings.HasPrefix(raw, "ws://"):
		rest = raw[len("ws://"):]
	case strings.HasPrefix(raw, "wss://"):
		u.secure = true
		rest = raw[len("wss://"):]
	default:
		return u, fmt.Errorf("wsclient: 地址必须以 ws:// 或 wss:// 开头: %q", raw)
	}

	// 切出 host 与 path
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		u.host, u.path = rest, "/"
	} else {
		u.host, u.path = rest[:slash], rest[slash:]
	}
	if u.host == "" {
		return u, fmt.Errorf("wsclient: 地址缺少主机: %q", raw)
	}
	// 没写端口时补默认端口（ws 80 / wss 443）
	if !strings.Contains(u.host, ":") {
		if u.secure {
			u.host += ":443"
		} else {
			u.host += ":80"
		}
	}
	if u.path == "" {
		u.path = "/"
	}
	return u, nil
}

// handshake 发 Upgrade 请求并校验响应。
func handshake(nc net.Conn, br *bufio.Reader, u target, header map[string]string) error {
	// 16 字节随机 key，按 RFC 转 base64
	var k [16]byte
	if _, err := rand.Read(k[:]); err != nil {
		return fmt.Errorf("wsclient: 随机数失败: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(k[:])

	var b strings.Builder
	b.Grow(256)
	b.WriteString("GET ")
	b.WriteString(u.path)
	b.WriteString(" HTTP/1.1\r\nHost: ")
	b.WriteString(u.host)
	b.WriteString("\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: ")
	b.WriteString(key)
	b.WriteString("\r\nSec-WebSocket-Version: 13\r\n")
	for k2, v := range header {
		b.WriteString(k2)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")

	if err := nc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(nc, b.String()); err != nil {
		return fmt.Errorf("wsclient: 发送握手请求失败: %w", err)
	}

	// 读状态行 + 头
	if err := nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	statusLine, err := readLine(br, maxHeader)
	if err != nil {
		return fmt.Errorf("wsclient: 读握手响应失败: %w", err)
	}
	// 期望 "HTTP/1.1 101 Switching Protocols"
	f := strings.Fields(statusLine)
	if len(f) < 2 {
		return fmt.Errorf("wsclient: 握手响应格式不对: %q", statusLine)
	}
	if f[1] != "101" {
		return fmt.Errorf("wsclient: 握手被拒绝 HTTP %s", f[1])
	}

	wantAccept := acceptKey(key)
	gotAccept := ""
	total := len(statusLine)
	for {
		line, err := readLine(br, maxHeader-total)
		if err != nil {
			return fmt.Errorf("wsclient: 读握手头失败: %w", err)
		}
		if line == "" {
			break // 空行 = 头结束
		}
		total += len(line)
		if i := strings.IndexByte(line, ':'); i > 0 {
			name := strings.TrimSpace(line[:i])
			val := strings.TrimSpace(line[i+1:])
			if strings.EqualFold(name, "Sec-WebSocket-Accept") {
				gotAccept = val
			}
		}
	}
	if gotAccept != wantAccept {
		return fmt.Errorf("wsclient: Sec-WebSocket-Accept 校验失败（拿到 %q）", gotAccept)
	}

	// 握手完成后清掉 deadline，后续由每次读写自己设
	return nc.SetDeadline(time.Time{})
}

// acceptKey 计算期望的 Sec-WebSocket-Accept。
func acceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readLine 读一行（不含 CRLF），并限制单行长度。
func readLine(br *bufio.Reader, limit int) (string, error) {
	if limit <= 0 || limit > maxHeader {
		limit = maxHeader
	}
	var buf []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			// 去掉行尾 \r
			if n := len(buf); n > 0 && buf[n-1] == '\r' {
				buf = buf[:n-1]
			}
			return string(buf), nil
		}
		if len(buf) >= limit {
			return "", errors.New("wsclient: 响应头过长")
		}
		buf = append(buf, b)
	}
}

// dialTCP 建带 TCP_NODELAY 的 TCP 连接。
//
// Nagle 会把小包攒到 ~40ms 才发，对 30ms 一帧的实时音频是致命的，
// 所以必须在握手之前就把这个选项设上。
func dialTCP(ctx context.Context, u target, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 15 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	return d.DialContext(ctx, "tcp", u.host)
}

// writeMessage 写一条完整消息（单帧，客户端必须掩码）。
func (c *conn) writeMessage(ctx context.Context, typ MessageType, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	var op byte
	switch typ {
	case MessageText:
		op = opText
	case MessageBinary:
		op = opBinary
	default:
		return fmt.Errorf("wsclient: 不支持的消息类型 %d", typ)
	}

	hdr := 0
	switch n := len(payload); {
	case n < 126:
		hdr = 2
	case n <= 0xFFFF:
		hdr = 4
	default:
		hdr = 10
	}

	need := hdr + 4 + len(payload)
	if cap(c.wbuf) < need {
		c.wbuf = make([]byte, need)
	}
	buf := c.wbuf[:need]

	buf[0] = 0x80 | op // FIN + opcode
	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	switch hdr {
	case 2:
		buf[1] = 0x80 | byte(len(payload))
	case 4:
		buf[1] = 0x80 | 126
		binary.BigEndian.PutUint16(buf[2:4], uint16(len(payload)))
	case 10:
		buf[1] = 0x80 | 127
		binary.BigEndian.PutUint64(buf[2:10], uint64(len(payload)))
	}
	copy(buf[hdr:hdr+4], maskKey[:])

	// 掩码 + 拷贝一步完成
	body := buf[hdr+4:]
	for i, b := range payload {
		body[i] = b ^ maskKey[i&3]
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = c.nc.SetWriteDeadline(dl)
	} else {
		_ = c.nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := c.nc.Write(buf); err != nil {
		return err
	}
	return nil
}

// readMessage 读一条完整消息，内部处理分片与控制帧。
func (c *conn) readMessage(ctx context.Context) (MessageType, []byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.nc.SetReadDeadline(dl)
	} else {
		_ = c.nc.SetReadDeadline(time.Time{})
	}

	var (
		msgType MessageType
		// 每条消息单独分配：解析出的 Envelope/Frame 会引用这块内存，
		// 而业务处理在另一个协程里，跨消息复用会互相踩。
		// 33 帧/秒的小对象，GC 完全跟得上。
		buf     = make([]byte, 0, 2048)
		started bool
	)
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return 0, nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0F
		if h[1]&0x80 != 0 {
			return 0, nil, errors.New("wsclient: 服务端帧不应带掩码")
		}
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint64(ext[:]))
		}
		if n < 0 || n > maxMessage {
			return 0, nil, fmt.Errorf("wsclient: 帧过大 %d", n)
		}

		// 控制帧可能夹在分片中间，必须就地处理
		if op >= opClose {
			ctrl := make([]byte, n)
			if _, err := io.ReadFull(c.br, ctrl); err != nil {
				return 0, nil, err
			}
			switch op {
			case opPing:
				_ = c.writeControl(opPong, ctrl)
				continue
			case opPong:
				continue
			case opClose:
				code, reason := 1005, ""
				if len(ctrl) >= 2 {
					code = int(binary.BigEndian.Uint16(ctrl[:2]))
					reason = string(ctrl[2:])
				}
				_ = c.writeControl(opClose, ctrl)
				return 0, nil, &CloseError{Code: code, Reason: reason}
			default:
				continue // 未知控制帧，忽略
			}
		}

		if op != opContinuation {
			if started {
				return 0, nil, errors.New("wsclient: 上一帧还没结束就来了新消息")
			}
			started = true
			switch op {
			case opText:
				msgType = MessageText
			case opBinary:
				msgType = MessageBinary
			default:
				return 0, nil, fmt.Errorf("wsclient: 未知操作码 %d", op)
			}
		} else if !started {
			return 0, nil, errors.New("wsclient: 收到孤立的续帧")
		}

		if int64(len(buf))+n > maxMessage {
			return 0, nil, errors.New("wsclient: 消息超过上限")
		}
		if n > 0 {
			off := len(buf)
			buf = append(buf, make([]byte, n)...)
			if _, err := io.ReadFull(c.br, buf[off:]); err != nil {
				return 0, nil, err
			}
		}
		if fin {
			return msgType, buf, nil
		}
	}
}

// writeControl 发送控制帧（不分片、可带负载）。
func (c *conn) writeControl(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	n := len(payload)
	if n > 125 {
		n = 125
	}
	buf := make([]byte, 2+4+n)
	buf[0] = 0x80 | op
	buf[1] = 0x80 | byte(n)
	var maskKey [4]byte
	_, _ = rand.Read(maskKey[:])
	copy(buf[2:6], maskKey[:])
	for i := 0; i < n; i++ {
		buf[6+i] = payload[i] ^ maskKey[i&3]
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := c.nc.Write(buf)
	return err
}

// close 发关闭帧并断开连接。
func (c *conn) close(code int, reason string) error {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	_ = c.writeControl(opClose, payload)
	// 把读端叫醒，避免读循环卡在 ReadFull 上
	_ = c.nc.SetReadDeadline(time.Now())
	return c.nc.Close()
}
