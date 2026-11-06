package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"xiaoai-go/internal/proto"
)

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

// TestHandshakeAndFrameEcho 验证：控制帧走 Text、音频帧走 Binary、负载原样。
func TestHandshakeAndFrameEcho(t *testing.T) {
	serverErr := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()

		// 1) 收 hello（必须是 Text）
		typ, data, err := c.Read(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		if typ != websocket.MessageText {
			serverErr <- fmt.Errorf("hello 应为 Text，实际 %v", typ)
			return
		}
		hello, err := proto.ParseEnvelope(data)
		if err != nil {
			serverErr <- err
			return
		}
		var hp proto.HelloPayload
		if err := hello.Decode(&hp); err != nil {
			serverErr <- err
			return
		}
		if hp.DeviceID != "lx06-test" {
			serverErr <- fmt.Errorf("device_id 丢失: %q", hp.DeviceID)
			return
		}

		ack, _ := proto.NewResp(hello.ID, proto.HelloAck{Protocol: proto.Version, HeartbeatMS: 30000})
		b, _ := ack.Marshal()
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			serverErr <- err
			return
		}

		// 2) 收音频帧（必须是 Binary），原样回声
		typ2, data2, err := c.Read(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		if typ2 != websocket.MessageBinary {
			serverErr <- fmt.Errorf("音频帧应为 Binary，实际 %v", typ2)
			return
		}
		if _, err := proto.ParseFrame(data2); err != nil {
			serverErr <- err
			return
		}
		if err := c.Write(ctx, websocket.MessageBinary, data2); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli, err := Dial(ctx, wsURL(ts), DialOptions{Token: "t"})
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer cli.Close("done")

	hello := proto.NewEnvelope(proto.TypeHello, "", "c:1")
	raw, err := json.Marshal(proto.HelloPayload{
		DeviceID: "lx06-test", Model: "LX06", Firmware: "1.94.13", Protocol: proto.Version,
		Caps: []string{"record", "play", "shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hello.Data = raw
	if err := cli.Send(ctx, hello); err != nil {
		t.Fatalf("发送 hello 失败: %v", err)
	}

	env, frame, err := cli.Recv(ctx)
	if err != nil {
		t.Fatalf("接收 ack 失败: %v", err)
	}
	if frame != nil {
		t.Fatal("期望控制帧，收到音频帧")
	}
	if env.Type != proto.TypeResp || env.Code != proto.CodeOK {
		t.Fatalf("ack 不匹配: %+v", env)
	}
	var ack proto.HelloAck
	if err := env.Decode(&ack); err != nil || ack.Protocol != proto.Version {
		t.Fatalf("ack 负载不匹配: %+v err=%v", ack, err)
	}

	payload := make([]byte, 960)
	for i := range payload {
		payload[i] = byte(i)
	}
	f := proto.NewAudioFrame(proto.FrameAudioUp, cli.NextSeq(), 16000, payload)
	if err := cli.SendFrame(ctx, f); err != nil {
		t.Fatalf("发送音频帧失败: %v", err)
	}

	env2, frame2, err := cli.Recv(ctx)
	if err != nil {
		t.Fatalf("接收回声失败: %v", err)
	}
	if env2 != nil || frame2 == nil {
		t.Fatal("期望音频帧回声")
	}
	if len(frame2.Payload) != len(payload) || frame2.Payload[0] != payload[0] || frame2.Seq != f.Seq {
		t.Fatalf("回声内容不一致: seq=%d len=%d", frame2.Seq, len(frame2.Payload))
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("服务端报错: %v", err)
	}
}

func TestDialRejectsWrongURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, "ws://127.0.0.1:1/ws", DialOptions{Timeout: time.Second}); err == nil {
		t.Fatal("连接不存在的地址应失败")
	}
}

func TestPingFrame(t *testing.T) {
	got := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		_, data, err := c.Read(context.Background())
		if err != nil {
			return
		}
		e, _ := proto.ParseEnvelope(data)
		got <- string(e.Type)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, err := Dial(ctx, wsURL(ts), DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close("done")

	if err := cli.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case typ := <-got:
		if typ != string(proto.TypePing) {
			t.Fatalf("期望 ping，实际 %s", typ)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端未收到 ping")
	}
}
