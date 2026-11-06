package audio

// 音频格式转换与杂项工具。
//
// 云端 TTS 各有各的输出格式（豆包给 pcm/mp3，Edge 给 mp3，一些自建服务给 wav），
// 但播放链路只吃一种：16bit 单声道 PCM。所有解码都收敛到这里，
// 新 provider 只需要调一个函数，不用各自实现一遍 WAV/MP3 解析。

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

// decodeWAVToMono 解 WAV 并降到单声道。采样率以文件头为准。
func decodeWAVToMono(b []byte) ([]byte, int, error) {
	pcm, rate, err := decodeWAV(b)
	if err != nil {
		return nil, 0, err
	}
	return pcm, rate, nil
}

// decodeMP3ToMono 解 MP3 并降到单声道。
//
// go-mp3 固定输出双声道交错 16bit，所以这里必须降混：
// 直接喂给 aplay（单声道）会把两声道当成一段更长的单声道音频，听起来是倍速人声。
func decodeMP3ToMono(b []byte) ([]byte, int, error) {
	dec, err := mp3.NewDecoder(bytes.NewReader(b))
	if err != nil {
		return nil, 0, fmt.Errorf("audio: MP3 解码失败: %w", err)
	}
	rate := dec.SampleRate()
	if rate <= 0 {
		rate = 24000
	}
	raw, err := io.ReadAll(io.LimitReader(dec, 64<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("audio: MP3 解码失败: %w", err)
	}
	// 每帧 4 字节（L16 + R16），降成 2 字节
	if len(raw) < 4 {
		return nil, 0, fmt.Errorf("audio: MP3 解出空音频")
	}
	n := len(raw) / 4
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		l := int16(binary.LittleEndian.Uint16(raw[i*4 : i*4+2]))
		r := int16(binary.LittleEndian.Uint16(raw[i*4+2 : i*4+4]))
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(int16((int32(l)+int32(r))/2)))
	}
	return out, rate, nil
}

// randomID 生成一个 UUIDv4 形式的请求号。
//
// 豆包要求 reqid / X-Api-Request-Id 全局唯一（重复会直接报错），
// 所以不能用时间戳凑，必须有随机位。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用时退化成时间，仍然保证"同一进程内不重复"
		binary.LittleEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// HasAudio 判断一段字节里有没有"真的声音"（不全是静音）。
//
// 用途：某些平台在音色 ID 写错时会返回 200 + 一段静音，光看状态码发现不了，
// 用户只会觉得"没声音"。预览接口拿它做一次兜底判断。
func HasAudio(pcm []byte) bool {
	if len(pcm) < 320 { // 不足 10ms
		return false
	}
	var peak int16
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak > 120 // 约 -48dBFS
}
