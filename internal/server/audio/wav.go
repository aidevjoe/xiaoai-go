package audio

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EncodeWAV 把 16bit 单声道 PCM 包成 WAV 字节流。
func EncodeWAV(pcm []byte, rate int) []byte {
	if rate <= 0 {
		rate = SampleRate
	}
	out := make([]byte, 44+len(pcm))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(pcm)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1) // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}

// SaveWAV 把一段识别到的语音落盘，返回文件路径。
//
// 只在后台打开"保存录音"时调用：调试识别错误时，能回放听到底说了什么，
// 比看日志猜有效得多。
func SaveWAV(dir string, pcm []byte) (string, error) {
	if dir == "" || len(pcm) == 0 {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("audio: 建录音目录失败: %w", err)
	}
	name := time.Now().Format("20060102-150405.000") + ".wav"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, EncodeWAV(pcm, SampleRate), 0o644); err != nil {
		return "", fmt.Errorf("audio: 写录音失败: %w", err)
	}
	return path, nil
}
