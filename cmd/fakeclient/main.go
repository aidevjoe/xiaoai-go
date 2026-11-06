// fakeclient 在本机模拟一台音箱，用于没有硬件时联调服务端。
//
//	go run ./cmd/fakeclient -server ws://127.0.0.1:4399/ws -id fake -audio
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"xiaoai-go/internal/client/wsclient"
	"xiaoai-go/internal/proto"
)

var (
	flagServer = flag.String("server", "ws://127.0.0.1:4399/ws", "服务端地址")
	flagID     = flag.String("id", "fake01", "设备 ID")
	flagToken  = flag.String("token", "", "鉴权 token")
	flagAudio  = flag.Bool("audio", false, "持续发送模拟音频帧（测带宽与帧率）")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cli, err := wsclient.Dial(ctx, *flagServer, wsclient.DialOptions{Token: *flagToken})
	if err != nil {
		log.Fatalf("连接失败: %v", err)
	}
	defer cli.Close("bye")

	hello := proto.NewEnvelope(proto.TypeHello, "", "c:1")
	raw, _ := json.Marshal(proto.HelloPayload{
		DeviceID: *flagID, Model: "FAKE-ESP", Firmware: "test", Version: "fake",
		Protocol: proto.Version, Caps: []string{"record", "play", "tone", "shell", "info"},
	})
	hello.Data = raw
	if err := cli.Send(ctx, hello); err != nil {
		log.Fatalf("发送 hello 失败: %v", err)
	}

	if *flagAudio {
		go sendAudio(ctx, cli)
	}

	for {
		env, frame, err := cli.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("读取失败: %v", err)
			return
		}
		if frame != nil {
			// 收到下行音频：打印时长估算（24k/16bit/mono）
			ms := len(frame.Payload) * 1000 / (24000 * 2)
			log.Printf("← 音频 %d 字节 ≈ %dms seq=%d", len(frame.Payload), ms, frame.Seq)
			continue
		}
		handle(ctx, cli, env)
	}
}

func handle(ctx context.Context, cli *wsclient.Client, e *proto.Envelope) {
	switch e.Type {
	case proto.TypeHelloAck:
		var ack proto.HelloAck
		_ = e.Decode(&ack)
		log.Printf("握手成功: 协议 v%d 帧长 %dms 心跳 %dms", ack.Protocol, ack.FrameMS, ack.HeartbeatMS)

	case proto.TypePong:
		log.Printf("pong rtt=%dms", proto.NowMS()-e.TS)

	case proto.TypeReq:
		log.Printf("← 指令 %s id=%s data=%s", e.Cmd, e.ID, string(e.Data))
		var resp *proto.Envelope
		switch e.Cmd {
		case proto.CmdDeviceInfo:
			r, _ := proto.NewResp(e.ID, proto.DeviceInfo{
				Model: "FAKE-ESP", Firmware: "test", UptimeSec: 42,
				MemFreeMB: 128, DiskFreeMB: 900, Load1: 0.1,
			})
			resp = r
		case proto.CmdPlayTone:
			start := time.Now()
			resp, _ = proto.NewResp(e.ID, map[string]any{"played_ms": time.Since(start).Milliseconds()})
		case proto.CmdRecordStart, proto.CmdRecordStop, proto.CmdPlayStart, proto.CmdPlayStop:
			resp, _ = proto.NewResp(e.ID, nil)
		case proto.CmdShellRun:
			resp, _ = proto.NewResp(e.ID, proto.ShellResult{Stdout: "fake-ok\n", ExitCode: 0})
		default:
			resp = proto.NewErrResp(e.ID, proto.CodeUnknownCmd, "fakeclient 不支持 "+e.Cmd)
		}
		resp.ID = e.ID
		if err := cli.Send(ctx, resp); err != nil {
			log.Printf("回包失败: %v", err)
		}

	case proto.TypeError:
		log.Printf("服务端错误: %d %s", e.Code, e.Msg)
	}
}

// sendAudio 以 33 帧/秒发送 30ms 的静音帧，模拟真实上行负载。
func sendAudio(ctx context.Context, cli *wsclient.Client) {
	t := time.NewTicker(30 * time.Millisecond)
	defer t.Stop()
	payload := make([]byte, 960)
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n++
			f := proto.NewAudioFrame(proto.FrameAudioUp, cli.NextSeq(), 16000, payload)
			f.TimestampUS = time.Now().UnixMicro()
			if err := cli.SendFrame(ctx, f); err != nil {
				log.Printf("发送音频失败: %v", err)
				return
			}
			if n%500 == 0 {
				fmt.Fprintf(os.Stderr, "已发送 %d 帧（≈%.1fs 音频）\n", n, float64(n)*0.03)
			}
		}
	}
}

var _ = websocket.MessageBinary
