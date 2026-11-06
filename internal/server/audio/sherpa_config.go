package audio

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SherpaConfig 描述模型文件位置与推理参数。
type SherpaConfig struct {
	ModelDir string // 模型根目录，其余路径留空时按约定推导

	VADModel string // silero_vad.onnx
	ASRDir   string // ASR 模型目录（含 model.int8.onnx 与 tokens.txt）
	// ASRKind 指定模型类型：sensevoice / paraformer；留空则按目录名推断。
	// 两种模型的推理图不同，必须选对，否则加载失败。
	ASRKind string

	KWSEncoder   string // encoder.onnx
	KWSDecoder   string // decoder.onnx
	KWSJoiner    string // joiner.onnx
	KWSTokens    string // tokens.txt
	KWSBpeModel  string // bpe.model
	KeywordsFile string // keywords.txt
	// KWSEnabled 为 true 才加载 KWS 模型。
	// 关键词文件是在引擎构造时读进去的，所以后台改唤醒词要重建引擎才生效。
	KWSEnabled bool

	// ASRLang 固定识别语种（zh/en/ja/ko/yue）；空=auto，让模型自己猜。
	// SenseVoice 在短促噪声片段上经常猜成英文/日文，中文场景务必固定 zh。
	ASRLang string

	TTSModel string // vits: model.onnx；matcha: model-steps-*.onnx
	// TTSKind 指定合成架构：vits / matcha；留空则按是否有声码器推断。
	TTSKind    string
	TTSVocoder string // matcha 专用声码器（vocos-22khz-univ.onnx 等）
	TTSTokens  string
	TTSLexicon string
	TTSDataDir string // espeak-ng-data（部分 VITS 模型需要）
	TTSDictDir string // jieba 词典目录（melo 等模型需要）

	Threads int // 推理线程数，4 核 NAS 建议 2
	Debug   int // 1 = 打印模型加载细节（排查用）

	VADThreshold  float32
	VADMinSilence float32 // 秒
	VADMinSpeech  float32 // 秒

	KWSScore     float32
	KWSThreshold float32
}

// Resolved 返回补齐默认路径后的副本。
// 调用方（服务端）需要先拿到确定的模型路径，才能在构造引擎前生成 keywords.txt。
func (c SherpaConfig) Resolved() SherpaConfig {
	c.withDefaults()
	return c
}

// withDefaults 按模型目录约定补齐路径。
func (c *SherpaConfig) withDefaults() {
	if c.Threads <= 0 {
		// 0 = 自动：按部署机器的核数决定。
		// 不写死固定值——同一份配置要能同时跑在 4 核 NAS 和 8 核小主机上。
		c.Threads = AutoThreads()
	}
	if c.VADThreshold == 0 {
		c.VADThreshold = 0.1
	}
	if c.VADMinSilence == 0 {
		c.VADMinSilence = 0.3
	}
	if c.VADMinSpeech == 0 {
		c.VADMinSpeech = 0.25
	}
	if c.KWSScore == 0 {
		c.KWSScore = 2.0
	}
	if c.KWSThreshold == 0 {
		c.KWSThreshold = 0.25
	}
	if c.ModelDir == "" {
		c.ModelDir = "models"
	}
	if c.VADModel == "" {
		c.VADModel = filepath.Join(c.ModelDir, "silero_vad.onnx")
	}
	// ---- KWS（关键词唤醒）：模型一般在 ModelDir 下的子目录里，按文件名发现 ----
	if c.KWSEncoder == "" || c.KWSDecoder == "" || c.KWSJoiner == "" {
		if dir := findKWSDir(c.ModelDir); dir != "" {
			if c.KWSEncoder == "" {
				c.KWSEncoder = pickOnnx(dir, "encoder")
			}
			if c.KWSDecoder == "" {
				c.KWSDecoder = pickOnnx(dir, "decoder")
			}
			if c.KWSJoiner == "" {
				c.KWSJoiner = pickOnnx(dir, "joiner")
			}
			if c.KWSTokens == "" {
				c.KWSTokens = filepath.Join(dir, "tokens.txt")
			}
			if c.KeywordsFile == "" {
				c.KeywordsFile = filepath.Join(dir, "keywords.txt")
			}
		}
	}

	// ---- 自动发现：让调用方只传 -models 也能跑起来 ----
	if c.ASRDir == "" {
		if d := findDirContaining(c.ModelDir, "model.int8.onnx", "model.onnx"); d != "" {
			if strings.Contains(strings.ToLower(d), "sense") {
				c.ASRDir = d
			}
		}
	}
	if c.TTSModel == "" {
		// 默认挑最快的（aishell3 8k，RTF≈0.2）；
		// 想要音质可显式传 -tts-model 指向 melo(44.1k) / fanchen(16k)，
		// 但它们在 4 核 CPU 上 RTF >1，会明显拖慢首字。
		for _, pref := range []string{"aishell3", "vits"} {
			if c.TTSModel = findModelIn(c.ModelDir, pref); c.TTSModel != "" {
				break
			}
		}
	}
	if c.TTSModel != "" {
		dir := filepath.Dir(c.TTSModel)
		if c.TTSTokens == "" {
			c.TTSTokens = filepath.Join(dir, "tokens.txt")
		}
		if c.TTSLexicon == "" {
			if p := filepath.Join(dir, "lexicon.txt"); fileExists(p) {
				c.TTSLexicon = p
			}
		}
		if c.TTSDictDir == "" {
			if p := filepath.Join(dir, "dict"); fileExistsDir(p) {
				c.TTSDictDir = p
			}
		}
		if c.TTSDataDir == "" {
			for _, p := range []string{
				filepath.Join(dir, "espeak-ng-data"),
				filepath.Join(c.ModelDir, "espeak-ng-data"),
			} {
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					c.TTSDataDir = p
					break
				}
			}
		}
	}
}

// findKWSDir 在模型根目录下找 KWS 模型所在的子目录（含 encoder*.onnx 的那一层）。
func findKWSDir(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			n := strings.ToLower(f.Name())
			if strings.HasPrefix(n, "encoder") && strings.HasSuffix(n, ".onnx") {
				return dir
			}
		}
	}
	return ""
}

// pickOnnx 在目录里按前缀找 onnx 文件，优先 int8（体积小、CPU 上更快）。
func pickOnnx(dir, prefix string) string {
	files, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var int8Path, anyPath string
	for _, f := range files {
		n := strings.ToLower(f.Name())
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".onnx") {
			continue
		}
		if strings.Contains(n, "int8") {
			if int8Path == "" {
				int8Path = filepath.Join(dir, f.Name())
			}
		} else if anyPath == "" {
			anyPath = filepath.Join(dir, f.Name())
		}
	}
	if int8Path != "" {
		return int8Path
	}
	return anyPath
}

// findDirContaining 在 root 下找一层子目录，目录里含任意一个指定文件。
func findDirContaining(root string, names ...string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		for _, n := range names {
			if fileExists(filepath.Join(dir, n)) {
				return dir
			}
		}
	}
	return ""
}

// findModelIn 找目录名含 needle 的子目录里的 onnx 模型。
// 注意：匹配的是**目录名**（如 vits-icefall-zh-aishell3），
// 文件名通常就叫 model.onnx，不含 "vits"。
func findModelIn(root, needle string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), needle) {
			continue
		}
		sub := filepath.Join(root, e.Name())
		// 优先 model.onnx / model.int8.onnx
		for _, name := range []string{"model.onnx", "model.int8.onnx"} {
			if p := filepath.Join(sub, name); fileExists(p) {
				return p
			}
		}
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(strings.ToLower(f.Name()), ".onnx") {
				return filepath.Join(sub, f.Name())
			}
		}
	}
	return ""
}

// ---- 音频格式转换与文件检查（纯 Go，可跨平台单测）----

func pcm16ToFloat(b []byte) []float32 {
	n := len(b) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(uint16(b[i*2]) | uint16(b[i*2+1])<<8)
		out[i] = float32(v) / 32768.0
	}
	return out
}

func floatToPCM16(s []float32) []byte {
	out := make([]byte, len(s)*2)
	for i, f := range s {
		if f > 1 {
			f = 1
		} else if f < -1 {
			f = -1
		}
		v := int16(f * 32767)
		out[i*2] = byte(uint16(v))
		out[i*2+1] = byte(uint16(v) >> 8)
	}
	return out
}

func fileExistsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func mustExist(label, p string) error {
	if !fileExists(p) {
		return fmt.Errorf("audio: %s 不存在: %s", label, p)
	}
	return nil
}

func pickFirst(paths ...string) string {
	for _, p := range paths {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// asrLang 把配置里的语种收敛成 SenseVoice 认识的值，默认中文。
func asrLang(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto":
		return "auto"
	case "zh", "en", "ja", "ko", "yue":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "zh"
	}
}

// AutoThreads 返回本机合适的推理线程数。
// 上限 8：再多的核在这种小模型上收益很小，反而容易和其他服务抢。
func AutoThreads() int {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// asrKind 决定用哪种 ASR 推理图。
// 显式配置优先；没配就看目录名（sherpa 的模型目录名里都带模型名），
// 都没有则默认 sensevoice（现有部署就是这个）。
func asrKind(explicit, dir string) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "paraformer":
		return "paraformer"
	case "sensevoice", "sense-voice", "sense":
		return "sensevoice"
	}
	if strings.Contains(strings.ToLower(dir), "paraformer") {
		return "paraformer"
	}
	return "sensevoice"
}

// ttsKind 决定用哪种 TTS 推理图。
// 显式配置优先；没配就看有没有声码器——只有 Matcha 需要单独声码器。
func ttsKind(explicit, vocoder string) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "matcha":
		return "matcha"
	case "vits":
		return "vits"
	}
	if strings.TrimSpace(vocoder) != "" {
		return "matcha"
	}
	return "vits"
}
