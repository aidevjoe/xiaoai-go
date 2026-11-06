package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 音频帧格式（小端）：
//
//	off  size  字段
//	0    2     magic "XA"
//	2    1     version
//	3    1     type     1=上行麦克风 2=下行播放 3=AEC 参考
//	4    4     seq      发送序号，用于丢帧检测
//	8    8     ts_us    发送时刻（微秒），用于端到端延迟测量
//	16   2     flags    bit0 语音开始 / bit1 流结束 / bit2 打断
//	18   2     sample_rate
//	20   1     channels
//	21   1     sample_fmt 0=S16LE
//	22   2     payload_len
//	24   N     payload  原始 PCM
const (
	Magic              = "XA"
	FrameVersion uint8 = 2
	HeaderSize         = 24
	MaxPayload   int   = 65535
)

// FrameType 区分方向。
type FrameType uint8

const (
	FrameAudioUp   FrameType = 1 // 音箱 → 服务端：麦克风
	FrameAudioDown FrameType = 2 // 服务端 → 音箱：播放
	FrameAECRef    FrameType = 3 // 预留：AEC 参考信号
)

func (t FrameType) String() string {
	switch t {
	case FrameAudioUp:
		return "audio_up"
	case FrameAudioDown:
		return "audio_down"
	case FrameAECRef:
		return "aec_ref"
	default:
		return fmt.Sprintf("frame_type(%d)", uint8(t))
	}
}

// FrameFlags 是音频帧的位标记。
type FrameFlags uint16

const (
	FlagSpeechStart FrameFlags = 1 << 0
	FlagStreamEnd   FrameFlags = 1 << 1
	FlagInterrupt   FrameFlags = 1 << 2
)

// Has 判断标记位。
func (f FrameFlags) Has(flag FrameFlags) bool { return f&flag != 0 }

// SampleFormat 目前只支持小端 16 位整数。
type SampleFormat uint8

const SampleS16LE SampleFormat = 0

// Frame 是一帧音频。
type Frame struct {
	Version     uint8
	Type        FrameType
	Seq         uint32
	TimestampUS int64
	Flags       FrameFlags
	SampleRate  uint16
	Channels    uint8
	Format      SampleFormat
	Payload     []byte
}

// NewAudioFrame 构造音频帧。
func NewAudioFrame(t FrameType, seq uint32, rate uint16, payload []byte) *Frame {
	return &Frame{
		Version:     FrameVersion,
		Type:        t,
		Seq:         seq,
		TimestampUS: 0, // 由调用方填，或用 SetNow
		SampleRate:  rate,
		Channels:    1,
		Format:      SampleS16LE,
		Payload:     payload,
	}
}

// Size 返回整帧字节数。
func (f *Frame) Size() int { return HeaderSize + len(f.Payload) }

// MarshalTo 把帧写入 dst，返回写入长度。dst 必须足够大（用 Size() 预分配）。
func (f *Frame) MarshalTo(dst []byte) (int, error) {
	if len(f.Payload) > MaxPayload {
		return 0, fmt.Errorf("proto: 音频负载 %d 超过上限 %d", len(f.Payload), MaxPayload)
	}
	if len(dst) < f.Size() {
		return 0, io.ErrShortBuffer
	}
	copy(dst[0:2], Magic)
	dst[2] = f.Version
	if dst[2] == 0 {
		dst[2] = FrameVersion
	}
	dst[3] = byte(f.Type)
	binary.LittleEndian.PutUint32(dst[4:8], f.Seq)
	binary.LittleEndian.PutUint64(dst[8:16], uint64(f.TimestampUS))
	binary.LittleEndian.PutUint16(dst[16:18], uint16(f.Flags))
	binary.LittleEndian.PutUint16(dst[18:20], f.SampleRate)
	dst[20] = f.Channels
	if dst[20] == 0 {
		dst[20] = 1
	}
	dst[21] = byte(f.Format)
	binary.LittleEndian.PutUint16(dst[22:24], uint16(len(f.Payload)))
	n := copy(dst[HeaderSize:], f.Payload)
	return HeaderSize + n, nil
}

// AppendTo 追加到 dst（append 语义，便于复用缓冲区）。
func (f *Frame) AppendTo(dst []byte) ([]byte, error) {
	if len(f.Payload) > MaxPayload {
		return dst, fmt.Errorf("proto: 音频负载 %d 超过上限 %d", len(f.Payload), MaxPayload)
	}
	start := len(dst)
	dst = append(dst, make([]byte, f.Size())...)
	if _, err := f.MarshalTo(dst[start:]); err != nil {
		return dst[:start], err
	}
	return dst, nil
}

// ParseFrame 解析一整帧（含头）。b 长度必须 ≥ HeaderSize。
func ParseFrame(b []byte) (*Frame, error) {
	if len(b) < HeaderSize {
		return nil, ErrShortFrame
	}
	if string(b[0:2]) != Magic {
		return nil, ErrBadMagic
	}
	payloadLen := int(binary.LittleEndian.Uint16(b[22:24]))
	if len(b) < HeaderSize+payloadLen {
		return nil, ErrShortFrame
	}
	f := &Frame{
		Version:     b[2],
		Type:        FrameType(b[3]),
		Seq:         binary.LittleEndian.Uint32(b[4:8]),
		TimestampUS: int64(binary.LittleEndian.Uint64(b[8:16])),
		Flags:       FrameFlags(binary.LittleEndian.Uint16(b[16:18])),
		SampleRate:  binary.LittleEndian.Uint16(b[18:20]),
		Channels:    b[20],
		Format:      SampleFormat(b[21]),
		Payload:     b[HeaderSize : HeaderSize+payloadLen],
	}
	if f.Version != FrameVersion {
		return nil, fmt.Errorf("%w: 收到 v%d，期望 v%d", ErrBadVersion, f.Version, FrameVersion)
	}
	return f, nil
}

// ReadFrame 从流里读一帧（先读头，再按长度读负载）。
// 注意：会分配新的 payload 切片；热路径上建议直接读整条消息后调用 ParseFrame。
func ReadFrame(r io.Reader) (*Frame, error) {
	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, err
	}
	if string(head[0:2]) != Magic {
		return nil, ErrBadMagic
	}
	payloadLen := int(binary.LittleEndian.Uint16(head[22:24]))
	buf := make([]byte, HeaderSize+payloadLen)
	copy(buf, head)
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, buf[HeaderSize:]); err != nil {
			return nil, err
		}
	}
	return ParseFrame(buf)
}

// WriteFrame 把帧写到流。
func WriteFrame(w io.Writer, f *Frame) error {
	buf, err := f.AppendTo(make([]byte, 0, f.Size()))
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// PCM16 把字节负载当作 int16 采样解析（仅用于特征提取，不做拷贝）。
func (f *Frame) PCM16() []int16 {
	n := len(f.Payload) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(f.Payload[i*2:]))
	}
	return out
}

var (
	ErrShortFrame  = errors.New("proto: 音频帧长度不足")
	ErrBadMagic    = errors.New("proto: 音频帧 magic 不匹配")
	ErrBadVersion  = errors.New("proto: 音频帧版本不匹配")
	ErrNotEnvelope = errors.New("proto: 不是控制帧")
	ErrBadEnvelope = errors.New("proto: 控制帧缺少 type")
)
