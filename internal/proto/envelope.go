// Package proto 定义音箱端与服务端之间的通信协议（v2）。
//
// 设计要点：
//   - 控制帧走 WebSocket Text，扁平 JSON（不再有 {"Request":{...}} 这类外层标签）
//   - 音频帧走 WebSocket Binary，24 字节裸头 + 原始 PCM
//     （旧协议把 PCM 编成十进制数字数组，体积膨胀 3.5~4 倍）
//   - 双向都能发请求：id 前缀 "c:" 表示客户端发起，"s:" 表示服务端发起
package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// 协议版本。握手时双方交换，服务端回交集。
const Version = 2

// MsgType 是控制帧的类型。
type MsgType string

const (
	TypeHello    MsgType = "hello"     // C→S 握手
	TypeHelloAck MsgType = "hello_ack" // S→C 握手应答
	TypeReq      MsgType = "req"       // 请求（bidirectional）
	TypeResp     MsgType = "resp"      // 应答
	TypeEvent    MsgType = "event"     // 事件（单向）
	TypePing     MsgType = "ping"
	TypePong     MsgType = "pong"
	TypeError    MsgType = "error" // 无法归类的错误（协议级）
)

// Envelope 是所有控制帧的统一外壳。
type Envelope struct {
	V    int             `json:"v"`
	Type MsgType         `json:"type"`
	ID   string          `json:"id,omitempty"`
	TS   int64           `json:"ts"` // 毫秒时间戳，用于测 RTT
	Cmd  string          `json:"cmd,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Code int             `json:"code,omitempty"`
	Msg  string          `json:"msg,omitempty"`
}

// NowMS 返回毫秒时间戳。
func NowMS() int64 { return time.Now().UnixMilli() }

// NewEnvelope 构造一个只填了版本与时间的基础帧。
func NewEnvelope(t MsgType, cmd, id string) *Envelope {
	return &Envelope{V: Version, Type: t, Cmd: cmd, ID: id, TS: NowMS()}
}

// NewReq 构造请求帧。data 为 nil 时不带 data 字段。
func NewReq(cmd, id string, data any) (*Envelope, error) {
	e := NewEnvelope(TypeReq, cmd, id)
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("proto: 序列化 %s 负载失败: %w", cmd, err)
		}
		e.Data = raw
	}
	return e, nil
}

// NewResp 构造成功应答。
func NewResp(id string, data any) (*Envelope, error) {
	e := NewEnvelope(TypeResp, "", id)
	e.Code = CodeOK
	e.Msg = "ok"
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("proto: 序列化应答负载失败: %w", err)
		}
		e.Data = raw
	}
	return e, nil
}

// NewErrResp 构造失败应答。
func NewErrResp(id string, code int, msg string) *Envelope {
	e := NewEnvelope(TypeResp, "", id)
	e.Code = code
	e.Msg = msg
	return e
}

// NewEvent 构造事件帧。
func NewEvent(name string, data any) (*Envelope, error) {
	e := NewEnvelope(TypeEvent, name, "")
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("proto: 序列化事件 %s 失败: %w", name, err)
		}
		e.Data = raw
	}
	return e, nil
}

// Decode 把 Data 解到 out。空 data 视为无操作。
func (e *Envelope) Decode(out any) error {
	if len(e.Data) == 0 || out == nil {
		return nil
	}
	if err := json.Unmarshal(e.Data, out); err != nil {
		return fmt.Errorf("proto: 解析 %s 的负载失败: %w", e.Cmd, err)
	}
	return nil
}

// IsReq 判断是否为请求。
func (e *Envelope) IsReq() bool { return e.Type == TypeReq }

// Marshal 序列化为 JSON 文本。
func (e *Envelope) Marshal() ([]byte, error) { return json.Marshal(e) }

// ParseEnvelope 解析控制帧。首字节不是 '{' 时返回 ErrNotEnvelope，
// 便于上层把二进制音频帧与文本帧分流。
func ParseEnvelope(b []byte) (*Envelope, error) {
	if len(b) == 0 || b[0] != '{' {
		return nil, ErrNotEnvelope
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("proto: 控制帧不是合法 JSON: %w", err)
	}
	if e.V == 0 {
		e.V = Version
	}
	if e.Type == "" {
		return nil, ErrBadEnvelope
	}
	return &e, nil
}
