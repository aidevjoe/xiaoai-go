package audio

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 模型文件自检。
//
// 为什么需要这一层：sherpa-onnx 是 C++ 库，模型加载失败时会 throw Ort::Exception，
// C++ 运行时随即 abort()，Go 的 recover 接不住，整个进程直接死。
// 真实踩过的坑：vits-melo-tts-zh_en/model.int8.onnx 是没下载的 Git LFS 指针（133 字节），
// 后台点了「重载引擎」后整个服务崩溃并进入重启循环。
//
// 这里只做便宜的检查（存在性 / 大小 / LFS 指针），
// 真正"能不能加载"由子进程探测兜底（见 cmd/server/probe.go）。

// minONNXBytes 是「这文件明显不是一个真模型」的下限。
// 只用来拦两类东西：Git LFS 指针（约 130 字节）和下载中断留下的空文件/碎片。
// 不能定得太大：KWS 的 joiner 是transducer 里最小的部件，正品也只有 ~65KB。
// 真正「能不能加载」由子进程探测兜底（见 cmd/server/probe.go）。
const minONNXBytes = 4 << 10

// lfsPointerPrefix 是 Git LFS 未下载时留在工作区的指针文件内容开头。
const lfsPointerPrefix = "version https://git-lfs"

// ValidateModelFiles 在把路径交给 sherpa 之前做一次自检。
// 返回的 error 直接面向用户，说明「哪个文件、为什么不行」。
func ValidateModelFiles(cfg SherpaConfig) error {
	cfg.withDefaults()

	if err := checkONNX("VAD 模型", cfg.VADModel); err != nil {
		return err
	}

	if cfg.ASRDir != "" {
		model := pickFirst(
			filepath.Join(cfg.ASRDir, "model.int8.onnx"),
			filepath.Join(cfg.ASRDir, "model.onnx"),
		)
		if model == "" {
			return fmt.Errorf("ASR 模型目录 %s 里既没有 model.int8.onnx 也没有 model.onnx", cfg.ASRDir)
		}
		if err := checkONNX("ASR 模型", model); err != nil {
			return err
		}
		if err := mustExist("ASR tokens", filepath.Join(cfg.ASRDir, "tokens.txt")); err != nil {
			return err
		}
	}

	if cfg.TTSModel != "" {
		if err := checkONNX("TTS 模型", cfg.TTSModel); err != nil {
			return err
		}
		// Matcha 把声码器单独拆出来，缺了会在 C++ 里崩，所以这里先拦
		if cfg.TTSVocoder != "" {
			if err := checkONNX("TTS 声码器", cfg.TTSVocoder); err != nil {
				return err
			}
		}
		if cfg.TTSTokens != "" {
			if err := mustExist("TTS tokens", cfg.TTSTokens); err != nil {
				return err
			}
		}
	}

	// KWS 是可选能力：路径不存在时引擎会降级成"无 KWS"，这里不拦。
	if fileExists(cfg.KWSEncoder) {
		for label, p := range map[string]string{
			"KWS encoder": cfg.KWSEncoder,
			"KWS decoder": cfg.KWSDecoder,
			"KWS joiner":  cfg.KWSJoiner,
		} {
			if err := checkONNX(label, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkONNX 检查一个 .onnx 文件是否值得交给推理引擎。
func checkONNX(label, p string) error {
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s 不存在: %s", label, p)
		}
		return fmt.Errorf("%s 读不到: %s (%v)", label, p, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%s 指向的是目录，需要具体到 .onnx 文件: %s", label, p)
	}
	if st.Size() >= minONNXBytes {
		return nil
	}
	// 小文件：读开头判断是不是 LFS 指针，给出能直接照做的提示
	if head, err := readHead(p, 256); err == nil {
		if strings.HasPrefix(strings.TrimSpace(string(head)), lfsPointerPrefix) {
			return fmt.Errorf("%s 是 Git LFS 指针文件，模型并没有真正下载: %s（仅 %d 字节）；"+
				"请执行 git lfs pull，或改指向同目录下已下载的 model.onnx", label, p, st.Size())
		}
	}
	return fmt.Errorf("%s 只有 %d 字节，文件不完整: %s", label, st.Size(), p)
}

func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:got], nil
}
