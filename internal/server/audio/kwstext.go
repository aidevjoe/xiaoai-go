package audio

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/mozillazg/go-pinyin"
)

// 服务端 KWS（sherpa-onnx keyword spotter）的关键词文件生成。
//
// sherpa 的 keywords.txt 格式是「带声调的拼音按声母/韵母拆开 + @汉字」，例如：
//
//	x iǎo h ēi @小黑
//	zh ōu w àng j ūn @周望军
//
// 也就是每个汉字要拆成「声母 token + 韵母 token」（零声母的字整个音节一个 token），
// 且每个 token 必须存在于模型自带的 tokens.txt 里。
// 手写这个文件不现实（后台一改唤醒词就得重来），所以这里按唤醒词自动生成。

// pinyinInitials 是模型 tokens.txt 里存在的声母。
// 注意 y / w 也按声母处理：望 wàng → w + àng。
var pinyinInitials = []string{
	"zh", "ch", "sh",
	"b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h",
	"j", "q", "x", "r", "z", "c", "s", "y", "w",
}

// splitSyllable 把带声调的音节拆成声母与韵母。
// 零声母（爱 ài、儿 ér、安 ān…）返回 ("", "ài")。
func splitSyllable(syl string) (initial, final string) {
	for _, in := range pinyinInitials {
		if strings.HasPrefix(syl, in) {
			rest := syl[len(in):]
			// 声母后面必须还有东西，否则说明这个音节本身就只有一个字母（如 "a"）
			if rest != "" {
				return in, rest
			}
		}
	}
	return "", syl
}

// syllableTokens 返回一个汉字对应的 token 列表。
func syllableTokens(char string) []string {
	args := pinyin.NewArgs()
	args.Style = pinyin.Tone // 带声调：航 → háng
	args.Heteronym = false   // 多音字取默认读音
	res := pinyin.Pinyin(char, args)
	if len(res) == 0 || len(res[0]) == 0 {
		return nil
	}
	syl := strings.ToLower(res[0][0])
	in, fin := splitSyllable(syl)
	if in == "" {
		return []string{fin}
	}
	return []string{in, fin}
}

// LoadTokenSet 读取 sherpa 模型的 tokens.txt，返回可用 token 集合。
func LoadTokenSet(tokensPath string) (map[string]bool, error) {
	f, err := os.Open(tokensPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	set := make(map[string]bool, 4096)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// 每行形如 "token id"
		if i := strings.LastIndexAny(line, " \t"); i > 0 {
			line = line[:i]
		}
		set[strings.TrimSpace(line)] = true
	}
	return set, sc.Err()
}

// BuildKeywordLine 把一个唤醒词转成 keywords.txt 里的一行。
// tokens 为该模型支持的 token 集合，用于提前发现「模型不认识这个字」。
func BuildKeywordLine(word string, tokens map[string]bool) (string, error) {
	word = strings.TrimSpace(word)
	if word == "" {
		return "", fmt.Errorf("唤醒词为空")
	}
	var parts []string
	for _, r := range word {
		c := string(r)
		// 只处理汉字；标点/空格直接跳过
		if r < 0x4E00 || r > 0x9FFF {
			continue
		}
		tks := syllableTokens(c)
		if len(tks) == 0 {
			return "", fmt.Errorf("汉字 %q 转不出拼音", c)
		}
		for _, t := range tks {
			if !tokens[t] {
				return "", fmt.Errorf("唤醒词 %q 里的 %q 需要 token %q，但模型 tokens.txt 里没有（可能不支持这个读音）", word, c, t)
			}
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("唤醒词 %q 里没有可用的汉字", word)
	}
	return strings.Join(parts, " ") + " @" + word, nil
}

// BuildKeywords 把多个唤醒词转成 keywords.txt 全文（去重、跳过无法转换的词）。
// 返回成功生成的关键词和逐条错误，便于后台把「哪个词不支持」告诉用户。
func BuildKeywords(words []string, tokensPath string) ([]string, []error, error) {
	tokens, err := LoadTokenSet(tokensPath)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 tokens.txt 失败: %w", err)
	}
	seen := make(map[string]bool)
	var lines, built []string
	var errs []error
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		line, err := BuildKeywordLine(w, tokens)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lines = append(lines, line)
		built = append(built, w)
	}
	if len(lines) == 0 {
		return nil, errs, fmt.Errorf("没有任何唤醒词能转换成 KWS 关键词")
	}
	return lines, errs, nil
}

// WriteKeywordsFile 生成 keywords.txt。返回实际写入的关键词，供日志展示。
// 内容不变时不重写，避免无意义地刷盘、也避免触发不必要的引擎重建。
func WriteKeywordsFile(path string, words []string, tokensPath string) ([]string, []error, error) {
	lines, errs, err := BuildKeywords(words, tokensPath)
	if err != nil {
		return nil, errs, err
	}
	content := strings.Join(lines, "\n") + "\n"
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return keywordTexts(lines), errs, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return nil, errs, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, errs, err
	}
	return keywordTexts(lines), errs, nil
}

// keywordTexts 从 "@汉字" 里取出汉字，仅用于日志。
func keywordTexts(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if i := strings.LastIndex(l, "@"); i >= 0 {
			out = append(out, strings.TrimSpace(l[i+1:]))
		}
	}
	return out
}

// PinyinOf 返回无声调拼音（空格分隔），用于"发音是否接近"的比较。
// 只取汉字，标点与空格忽略。
func PinyinOf(text string) string {
	args := pinyin.NewArgs()
	args.Style = pinyin.Normal
	args.Heteronym = false
	var parts []string
	for _, r := range text {
		if r < 0x4E00 || r > 0x9FFF {
			continue
		}
		res := pinyin.Pinyin(string(r), args)
		if len(res) > 0 && len(res[0]) > 0 {
			parts = append(parts, res[0][0])
		}
	}
	return strings.Join(parts, " ")
}
