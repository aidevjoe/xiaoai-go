package main

// 引擎热替换的引用计数。
//
// 背景：引擎是 C++ 对象（sherpa-onnx），Close() 会真正释放底层内存。
// 旧实现是"换引擎 → 睡 2 秒 → 旧引擎 Close()"，但这两秒里：
//   - 设备管道的 goroutine 仍持有旧引擎（它并不知道引擎换了）
//   - 正在合成的一句话可能还没念完
// 于是 Close 之后再进来一次调用，就是访问已释放的 C 对象——直接崩溃，
// 而且崩在 cgo 里，堆栈完全看不出业务上下文。
//
// 改成引用计数：谁要用引擎谁 acquire，用完 release；换引擎只"注记退休"，
// 等最后一个使用者放手才真正 Close。谁持有引用一目了然。

import (
	"sync"

	"xiaoai-go/internal/server/audio"
)

// engineHandle 是带引用计数的引擎句柄。
type engineHandle struct {
	mu      sync.Mutex
	eng     audio.Engine
	refs    int  // 当前引用数；创建时服务端自己持有一份
	retired bool // 已被替换，等引用归零就关
	closed  bool
}

func newEngineHandle(eng audio.Engine) *engineHandle {
	h := &engineHandle{eng: eng}
	if eng != nil {
		h.refs = 1 // 服务端持有
	}
	return h
}

// acquire 取一份引用。返回 false 表示引擎已不可用（未加载/已退休关闭）。
func (h *engineHandle) acquire() (audio.Engine, bool) {
	if h == nil {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.eng == nil || h.closed || h.retired {
		return nil, false
	}
	h.refs++
	return h.eng, true
}

// release 归还一份引用；若已退休且无人使用，则真正关闭引擎。
func (h *engineHandle) release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.refs > 0 {
		h.refs--
	}
	var toClose audio.Engine
	if h.refs == 0 && h.retired && !h.closed {
		h.closed = true
		toClose = h.eng
	}
	h.mu.Unlock()
	if toClose != nil {
		_ = toClose.Close()
	}
}

// retire 标记退休并归还服务端自己那份引用。
func (h *engineHandle) retire() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.retired = true
	if h.refs > 0 {
		h.refs--
	}
	var toClose audio.Engine
	if h.refs == 0 && !h.closed {
		h.closed = true
		toClose = h.eng
	}
	h.mu.Unlock()
	if toClose != nil {
		_ = toClose.Close()
	}
}

// raw 取引擎指针（不加引用）。只给"看一眼状态"的只读场景用，
// 真正要跑推理必须走 acquireEngine。
func (h *engineHandle) raw() audio.Engine {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.eng
}

// acquireEngine 取当前引擎 + 归还函数。调用方必须 defer release()。
func (s *server) acquireEngine() (audio.Engine, func(), bool) {
	s.engMu.RLock()
	h := s.engine
	s.engMu.RUnlock()
	if h == nil {
		return nil, func() {}, false
	}
	eng, ok := h.acquire()
	if !ok {
		return nil, func() {}, false
	}
	return eng, h.release, true
}
