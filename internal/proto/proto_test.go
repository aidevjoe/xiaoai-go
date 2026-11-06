package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvelopeIsFlatJSON(t *testing.T) {
	e, err := NewReq(CmdRecordStart, "s:1", RecordStartPayload{SampleRate: 16000, FrameMS: 30, Channels: 1})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	s := string(b)
	// 必须是扁平结构：{"v":2,"type":"req",...}
	if !strings.HasPrefix(s, `{"v":2,"type":"req"`) {
		t.Fatalf("控制帧不是扁平 JSON: %s", s)
	}
	// 不能出现旧协议的外层标签
	for _, bad := range []string{`"Request":`, `"Stream":`} {
		if strings.Contains(s, bad) {
			t.Fatalf("控制帧仍带旧协议外层标签 %s: %s", bad, s)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	orig, _ := NewEvent(EvPlaying, PlayingState{State: "playing"})
	orig.TS = 1700000000123
	b, _ := orig.Marshal()

	got, err := ParseEnvelope(b)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Type != TypeEvent || got.Cmd != EvPlaying {
		t.Fatalf("类型/命令不匹配: %+v", got)
	}
	var st PlayingState
	if err := got.Decode(&st); err != nil {
		t.Fatalf("解码负载失败: %v", err)
	}
	if st.State != "playing" {
		t.Fatalf("负载不匹配: %+v", st)
	}
}

func TestParseEnvelopeRejectsBinary(t *testing.T) {
	if _, err := ParseEnvelope([]byte(Magic + "\x02\x01")); err != ErrNotEnvelope {
		t.Fatalf("二进制帧应返回 ErrNotEnvelope，实际 %v", err)
	}
	if _, err := ParseEnvelope([]byte(`{"v":2}`)); err != ErrBadEnvelope {
		t.Fatalf("缺 type 应返回 ErrBadEnvelope，实际 %v", err)
	}
}

func TestErrResp(t *testing.T) {
	e := NewErrResp("s:9", CodeUnsupported, "设备不支持")
	b, _ := e.Marshal()
	got, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != CodeUnsupported || got.Msg != "设备不支持" || got.ID != "s:9" {
		t.Fatalf("错误应答不匹配: %+v", got)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	payload := make([]byte, 960) // 30ms @16k mono s16
	for i := range payload {
		payload[i] = byte(i)
	}
	f := NewAudioFrame(FrameAudioUp, 42, 16000, payload)
	f.TimestampUS = 1700000000123456
	f.Flags = FlagSpeechStart

	buf, err := f.AppendTo(nil)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if len(buf) != HeaderSize+len(payload) {
		t.Fatalf("长度不对: got %d want %d", len(buf), HeaderSize+len(payload))
	}
	if string(buf[0:2]) != Magic {
		t.Fatalf("magic 不对: %q", buf[0:2])
	}

	got, err := ParseFrame(buf)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Type != FrameAudioUp || got.Seq != 42 || got.SampleRate != 16000 {
		t.Fatalf("头部字段不匹配: %+v", got)
	}
	if got.TimestampUS != f.TimestampUS || got.Flags != FlagSpeechStart {
		t.Fatalf("时间戳/标记不匹配: %+v", got)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatal("负载不一致")
	}
}

func TestFrameIsRawPCMNotJSON(t *testing.T) {
	// 关键回归：音频必须走二进制裸 PCM，不能被编成 [1,2,3...] 这种数字数组
	payload := []byte{0x01, 0x02, 0xff, 0x7f}
	f := NewAudioFrame(FrameAudioDown, 1, 24000, payload)
	buf, _ := f.AppendTo(nil)

	if buf[0] == '{' {
		t.Fatal("音频帧被编成了 JSON")
	}
	// 头部之后就是原始字节
	if !bytes.Equal(buf[HeaderSize:], payload) {
		t.Fatalf("负载不是裸 PCM: % x", buf[HeaderSize:])
	}
	// 体积应严格等于 24 + N（旧协议会膨胀 3.5~4 倍）
	if len(buf) != 28 {
		t.Fatalf("音频帧体积异常: %d", len(buf))
	}
}

func TestFrameErrors(t *testing.T) {
	if _, err := ParseFrame([]byte("XA")); err != ErrShortFrame {
		t.Fatalf("短帧应报 ErrShortFrame，实际 %v", err)
	}
	if _, err := ParseFrame([]byte("ZZ" + strings.Repeat("\x00", 30))); err != ErrBadMagic {
		t.Fatalf("magic 错应报 ErrBadMagic，实际 %v", err)
	}

	// 版本不匹配
	f := NewAudioFrame(FrameAudioUp, 1, 16000, []byte{1, 2})
	buf, _ := f.AppendTo(nil)
	buf[2] = 9
	if _, err := ParseFrame(buf); err == nil {
		t.Fatal("版本不匹配应报错")
	}

	// 负载长度超出实际数据
	f2 := NewAudioFrame(FrameAudioUp, 1, 16000, []byte{1, 2})
	buf2, _ := f2.AppendTo(nil)
	binary.LittleEndian.PutUint16(buf2[22:24], 100)
	if _, err := ParseFrame(buf2); err != ErrShortFrame {
		t.Fatalf("截断帧应报 ErrShortFrame，实际 %v", err)
	}
}

func TestFrameOversizePayload(t *testing.T) {
	f := NewAudioFrame(FrameAudioUp, 1, 16000, make([]byte, MaxPayload+1))
	if _, err := f.AppendTo(nil); err == nil {
		t.Fatal("超长负载应报错")
	}
}

func TestReadWriteFrame(t *testing.T) {
	f := NewAudioFrame(FrameAudioDown, 7, 24000, []byte("pcm-payload"))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 7 || string(got.Payload) != "pcm-payload" {
		t.Fatalf("读写不一致: %+v", got)
	}
}

func TestFrameFlags(t *testing.T) {
	f := FlagSpeechStart | FlagStreamEnd
	if !f.Has(FlagSpeechStart) || !f.Has(FlagStreamEnd) {
		t.Fatal("标记位判定错误")
	}
	if f.Has(FlagInterrupt) {
		t.Fatal("不该包含 FlagInterrupt")
	}
}

// 基准：验证编解码开销足够低（目标 <1µs/帧，33 帧/秒时 CPU 占用可忽略）
func BenchmarkFrameEncode(b *testing.B) {
	payload := make([]byte, 960)
	f := NewAudioFrame(FrameAudioUp, 1, 16000, payload)
	dst := make([]byte, HeaderSize+len(payload))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.MarshalTo(dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFrameParse(b *testing.B) {
	payload := make([]byte, 960)
	f := NewAudioFrame(FrameAudioUp, 1, 16000, payload)
	buf, _ := f.AppendTo(nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseFrame(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnvelopeParse(b *testing.B) {
	e, _ := NewReq(CmdPlayStart, "s:1", PlayStartPayload{SampleRate: 24000, PrebufferMS: 120})
	buf, _ := e.Marshal()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseEnvelope(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnvelopeMarshal(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, _ := NewResp("s:1", DeviceInfo{Model: "LX06", Firmware: "1.94.13"})
		if _, err := json.Marshal(e); err != nil {
			b.Fatal(err)
		}
	}
}
