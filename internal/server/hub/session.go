// Package hub 管理所有已连接音箱的会话。
//
// 约束：一条 WebSocket 连接只允许一个写者（gorilla/coder 都不支持并发写），
// 因此所有下行都收敛到 Session 的写锁里；请求/应答用 pending 表配对。
package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"xiaoai-go/internal/proto"
)

var (
	ErrOffline       = errors.New("hub: 设备离线")
	ErrTimeout       = errors.New("hub: 等待设备应答超时")
	ErrAlreadyOnline = errors.New("hub: 同一设备重复连接")
)

// Session 是一个在线设备。
type Session struct {
	ID       string
	Conn     *websocket.Conn
	Hello    proto.HelloPayload
	Remote   string
	Since    time.Time
	lastSeen atomic.Int64 // unix ms

	wmu     sync.Mutex
	scratch []byte
	seq     atomic.Uint32
	reqSeq  atomic.Uint32

	pmu     sync.Mutex
	pending map[string]chan *proto.Envelope

	closeOnce sync.Once
	done      chan struct{}
}

// NewSession 构造会话。
func NewSession(id string, conn *websocket.Conn, hello proto.HelloPayload, remote string) *Session {
	s := &Session{
		ID:      id,
		Conn:    conn,
		Hello:   hello,
		Remote:  remote,
		Since:   time.Now(),
		pending: make(map[string]chan *proto.Envelope),
		done:    make(chan struct{}),
		scratch: make([]byte, 0, proto.HeaderSize+2048),
	}
	s.Touch()
	return s
}

// Touch 刷新活跃时间。
func (s *Session) Touch() { s.lastSeen.Store(proto.NowMS()) }

// LastSeen 返回最后活跃时间。
func (s *Session) LastSeen() time.Time { return time.UnixMilli(s.lastSeen.Load()) }

// Done 在会话关闭时关闭。
func (s *Session) Done() <-chan struct{} { return s.done }

// SendEnvelope 发送控制帧。
func (s *Session) SendEnvelope(ctx context.Context, e *proto.Envelope) error {
	b, err := e.Marshal()
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.Conn.Write(ctx, websocket.MessageText, b)
}

// SendFrame 发送音频帧。
func (s *Session) SendFrame(ctx context.Context, f *proto.Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.scratch, _ = f.AppendTo(s.scratch[:0])
	return s.Conn.Write(ctx, websocket.MessageBinary, s.scratch)
}

// NextAudioSeq 返回下行音频帧序号。
func (s *Session) NextAudioSeq() uint32 { return s.seq.Add(1) }

// Request 向设备发请求并等应答。
func (s *Session) Request(ctx context.Context, cmd string, payload any, timeout time.Duration) (*proto.Envelope, error) {
	id := fmt.Sprintf("s:%d", s.reqSeq.Add(1))
	req, err := proto.NewReq(cmd, id, payload)
	if err != nil {
		return nil, err
	}

	ch := make(chan *proto.Envelope, 1)
	s.pmu.Lock()
	s.pending[id] = ch
	s.pmu.Unlock()
	defer func() {
		s.pmu.Lock()
		delete(s.pending, id)
		s.pmu.Unlock()
	}()

	if err := s.SendEnvelope(ctx, req); err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case resp := <-ch:
		if resp.Code != proto.CodeOK {
			return resp, fmt.Errorf("hub: %s 失败(%d): %s", cmd, resp.Code, resp.Msg)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, ErrTimeout
	case <-s.done:
		return nil, ErrOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Deliver 把收到的应答交给等待方。返回是否有人认领。
func (s *Session) Deliver(e *proto.Envelope) bool {
	s.pmu.Lock()
	ch, ok := s.pending[e.ID]
	s.pmu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- e:
	default:
	}
	return true
}

// Close 关闭会话（幂等）。
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		s.Conn.Close(websocket.StatusNormalClosure, reason)
	})
}

// Hub 是会话注册表。
type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	// OnRegister/OnUnregister 由上层挂勾子（例如写日志、推后台事件）
	OnRegister   func(*Session)
	OnUnregister func(*Session)
}

// New 构造 Hub。
func New() *Hub {
	return &Hub{sessions: make(map[string]*Session)}
}

// Register 注册会话；同 ID 重复连接时踢掉旧的（设备重启最常见）。
func (h *Hub) Register(s *Session) {
	h.mu.Lock()
	old := h.sessions[s.ID]
	h.sessions[s.ID] = s
	h.mu.Unlock()

	if old != nil {
		old.Close("replaced by new connection")
		if h.OnUnregister != nil {
			h.OnUnregister(old)
		}
	}
	if h.OnRegister != nil {
		h.OnRegister(s)
	}
}

// Unregister 注销会话（只有当前会话才删，避免误删新连接）。
func (h *Hub) Unregister(s *Session) {
	h.mu.Lock()
	if cur, ok := h.sessions[s.ID]; ok && cur == s {
		delete(h.sessions, s.ID)
		needHook := true
		h.mu.Unlock()
		if needHook && h.OnUnregister != nil {
			h.OnUnregister(s)
		}
		return
	}
	h.mu.Unlock()
}

// Get 按 ID 取会话。
func (h *Hub) Get(id string) (*Session, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.sessions[id]
	if !ok {
		return nil, ErrOffline
	}
	return s, nil
}

// List 返回所有会话（快照）。
func (h *Hub) List() []*Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// Count 返回在线设备数。
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}
