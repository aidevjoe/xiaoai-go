package main

// 设备原生 TTS 的本地缓存。
//
// 固定应答（"我在""诶""好的"）每次都要走 ubus 云端合成，0.5~1 秒。
// 服务端拿不到这段音频（是设备自己生成的），所以缓存只能放设备上。
//
// 放在 /tmp（tmpfs，内存盘）：重启即清空，不占 /data 那点 flash。
// 一个 mp3 约 30~60KB，几十条也就一两 MB。
//
// 只缓存短句：长回答内容不重复，缓存只会白占内存。

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ttsCacheDir 是缓存目录。用 /tmp 下的子目录，明确表达"可随时丢失"。
const ttsCacheDir = "/tmp/xiaoai-tts"

// ttsCacheMaxFiles 缓存条数上限（约 1~2MB）。
const ttsCacheMaxFiles = 40

// ttsCacheMaxRunes 超过这个长度不缓存。
// 放宽到 120：流式回答的一段可能到 30~60 字，预合成后要能命中缓存才有意义；
// 目录里最多留 ttsCacheMaxFiles 个文件，长一点也只是多占几百 KB 内存盘。
const ttsCacheMaxRunes = 120

// ttsPathRe 从 ubus 输出里抠出 mp3 路径。
// 返回形如 {"code":0,"info":"{ \"path\": \"\/tmp\/tts\/xxx.mp3\" }"}
var ttsPathRe = regexp.MustCompile(`"path"\s*:\s*"([^"]+)"`)

// ttsCachePath 返回某个文本的缓存文件路径；不该缓存时返回空串。
func ttsCachePath(text string) string {
	if len([]rune(strings.TrimSpace(text))) > ttsCacheMaxRunes {
		return ""
	}
	sum := sha1.Sum([]byte(text))
	return filepath.Join(ttsCacheDir, hex.EncodeToString(sum[:])+".mp3")
}

// cachedTTS 取缓存；文件存在且不是空壳才算命中。
func cachedTTS(text string) (string, bool) {
	p := ttsCachePath(text)
	if p == "" {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() < 1024 {
		return "", false
	}
	return p, true
}

// saveTTS 把合成结果复制进缓存目录（原文件在 /tmp/tts，不复制会一直堆积）。
func saveTTS(src, text string) string {
	dst := ttsCachePath(text)
	if dst == "" {
		return src
	}
	if err := os.MkdirAll(ttsCacheDir, 0o755); err != nil {
		return src
	}
	if err := copyFile(src, dst); err != nil {
		return src
	}
	_ = os.Remove(src)
	pruneTTSCache()
	return dst
}

// pruneTTSCache 超过上限就按修改时间删最旧的。
func pruneTTSCache() {
	entries, err := os.ReadDir(ttsCacheDir)
	if err != nil || len(entries) <= ttsCacheMaxFiles {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp3") {
			continue
		}
		if info, err := e.Info(); err == nil {
			items = append(items, item{filepath.Join(ttsCacheDir, e.Name()), info.ModTime()})
		}
	}
	if len(items) <= ttsCacheMaxFiles {
		return
	}
	// 按时间升序，删掉多出来的最旧那些
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].mod.Before(items[j-1].mod); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	for i := 0; i < len(items)-ttsCacheMaxFiles; i++ {
		_ = os.Remove(items[i].path)
	}
}

// parseTTSPath 从 ubus 的返回里取出音频路径。取不到（或路径不可信）返回空串。
//
// ubus 返回的是「JSON 里套了一层 JSON 字符串」，所以真正的引号是转义的：
//
//	{"code":0,"info":"{ \"path\": \"\/tmp\/tts\/xxx.mp3\" }"}
//
// 先还原 `\"` 再匹配，比写一个到处是转义的表达式清楚得多。
func parseTTSPath(out string) string {
	s := strings.ReplaceAll(out, `\"`, `"`)
	m := ttsPathRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	// JSON 里斜杠被转义成 \/ ，另外可能还有 \" 之类
	p := strings.ReplaceAll(m[1], `\/`, "/")
	p = strings.ReplaceAll(p, `\`, "")
	return strings.TrimSpace(p)
}

// ttsPathPrefix 是允许播放的目录前缀。设备原生 TTS 把音频写在 /tmp/tts/。
// 做成变量只为测试能指向临时目录——生产路径永远是 /tmp/。
var ttsPathPrefix = "/tmp/"

// validateTTSPath 检查 ubus 给的路径是不是"一个真的音频文件"。
//
// 为什么值得多这一步：这段字符串来自设备侧的另一个进程，格式一旦变了
// （或者返回了你意想不到的东西），原来的实现会拿着这个路径直接去
// `miplayer -f <path>`——播放器卡住几秒、日志里只有一句"播放失败"。
// 现在不合法就返回空串，走脚本回退路径，并且把原始输出留给调用方打日志。
func validateTTSPath(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	// 只认 /tmp 下的文件：设备原生 TTS 就是把音频写到 /tmp/tts/。
	// 放宽到任意路径等于把"播放什么"的决定权交给外部进程。
	clean := filepath.Clean(p)
	if !strings.HasPrefix(clean, ttsPathPrefix) {
		return false
	}
	st, err := os.Stat(clean)
	if err != nil || !st.Mode().IsRegular() || st.Size() < 1024 {
		return false
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
