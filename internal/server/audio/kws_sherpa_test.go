//go:build linux && cgo && sherpa

// KWS 集成测试：需要真实的 sherpa-onnx 库与 KWS 模型，所以只在容器里跑：
//
//	docker exec xa-build sh -c 'go test -tags sherpa -run TestKWS -v ./internal/server/audio/'
//
// 为什么必须有这个测试：keywords.txt 的格式（带声调拼音按声母/韵母拆开）
// 一旦写错，sherpa 不会报错，只是永远不命中——纯靠人耳试是发现不了的。
package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

const kwsModelRel = "../../../models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"

// newKWSOnlyEngine 只加载 KWS（+ VAD）的引擎。
// ModelDir 用空目录、其余路径显式给出，这样自动发现不会把 ASR/TTS 也拉进来，
// 测试才不会为了验 KWS 等十几秒加载 200MB 模型。
func newKWSOnlyEngine(t *testing.T, keywordsFile string) *SherpaEngine {
	t.Helper()
	dir, err := filepath.Abs(kwsModelRel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tokens.txt")); err != nil {
		t.Skip("KWS 模型未下载，跳过")
	}
	pick := func(prefix string) string {
		p := pickOnnx(dir, prefix)
		if p == "" {
			t.Fatalf("KWS 模型缺少 %s*.onnx", prefix)
		}
		return p
	}
	cfg := SherpaConfig{
		ModelDir:     t.TempDir(), // 空的，避免自动发现别的模型
		VADModel:     filepath.Join(filepath.Dir(dir), "silero_vad.onnx"),
		KWSEnabled:   true,
		KWSEncoder:   pick("encoder"),
		KWSDecoder:   pick("decoder"),
		KWSJoiner:    pick("joiner"),
		KWSTokens:    filepath.Join(dir, "tokens.txt"),
		KeywordsFile: keywordsFile,
	}.Resolved()
	e, err := NewSherpaEngine(cfg)
	if err != nil {
		t.Fatalf("加载引擎失败: %v", err)
	}
	se := e.(*SherpaEngine)
	if se.kws == nil {
		t.Fatal("KWS 没有加载（kws == nil）")
	}
	return se
}

// readWavPCM 读 16bit PCM 的 wav，返回跳过头部后的裸 PCM。
func readWavPCM(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" {
		t.Fatalf("%s 不是 RIFF wav", path)
	}
	// 逐块找到 data
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		if id == "data" {
			end := off + 8 + size
			if end > len(b) {
				end = len(b)
			}
			return b[off+8 : end]
		}
		off += 8 + size + (size & 1)
	}
	t.Fatalf("%s 里没有 data 块", path)
	return nil
}

// feedFrames 按 30ms（960 字节）切帧喂进去，返回命中的关键词。
func feedFrames(t *testing.T, e *SherpaEngine, pcm []byte) []string {
	t.Helper()
	const frameBytes = 960 // 16kHz * 30ms * 2B
	var hits []string
	for i := 0; i+frameBytes <= len(pcm); i += frameBytes {
		kw, err := e.DetectKeyword(pcm[i : i+frameBytes])
		if err != nil {
			t.Fatalf("DetectKeyword 失败: %v", err)
		}
		if kw != "" {
			hits = append(hits, kw)
		}
	}
	return hits
}

// 用模型自带的测试音频验证 KWS 真的能命中，证明模型文件与引擎接线是对的。
func TestKWSDetect_TestWavs(t *testing.T) {
	dir, _ := filepath.Abs(kwsModelRel)
	kwFile := filepath.Join(dir, "test_wavs", "test_keywords.txt")
	e := newKWSOnlyEngine(t, kwFile)
	defer e.Close()

	wavs, _ := filepath.Glob(filepath.Join(dir, "test_wavs", "*.wav"))
	if len(wavs) == 0 {
		t.Skip("没有测试音频")
	}
	total := 0
	for _, w := range wavs {
		pcm := readWavPCM(t, w)
		hits := feedFrames(t, e, pcm)
		if len(hits) > 0 {
			total++
			t.Logf("%s 命中: %v", filepath.Base(w), hits)
		} else {
			t.Logf("%s 未命中", filepath.Base(w))
		}
	}
	if total == 0 {
		t.Fatal("所有测试音频都没命中：模型或 keywords 文件格式有问题")
	}
	t.Logf("共 %d/%d 个音频命中", total, len(wavs))
}

// 我们自己生成的 keywords.txt（后台唤醒词 → 拼音 token）必须能被 sherpa 接受。
// 这里只验证"能加载且不误触发"，真正的唤醒命中要有真人喊话才能测。
func TestKWSDetect_GeneratedKeywords(t *testing.T) {
	dir, _ := filepath.Abs(kwsModelRel)
	kwPath := filepath.Join(t.TempDir(), "keywords.txt")
	words, errs, err := WriteKeywordsFile(kwPath, []string{"小黑", "你好小黑"}, filepath.Join(dir, "tokens.txt"))
	if err != nil {
		t.Fatalf("生成关键词失败: %v（局部错误: %v）", err, errs)
	}
	t.Logf("生成的关键词: %v", words)

	e := newKWSOnlyEngine(t, kwPath)
	defer e.Close()

	// 喂一段无关语音（模型自带的其他唤醒词音频），不应命中"小黑"
	pcm := readWavPCM(t, filepath.Join(dir, "test_wavs", "0.wav"))
	if hits := feedFrames(t, e, pcm); len(hits) > 0 {
		t.Fatalf("无关音频误命中: %v", hits)
	}
}
