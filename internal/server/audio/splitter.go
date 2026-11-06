package audio

import (
	"strings"
	"unicode"
)

// Sanitize 清理不适合朗读的字符。
//
// 模型经常返回表情、markdown 强调符、列表符号，TTS 会把它们念出来
// 或者直接合成失败（"星号星号"、怪音）。这里统一剥掉。
func Sanitize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case isEmoji(r):
			continue
		case r == '*' || r == '#' || r == '`' || r == '~' || r == '_' || r == '|':
			continue // markdown 强调/代码/表格符号
		case r == '【' || r == '】' || r == '[' || r == ']':
			continue
		case r == '\u200b' || r == '\ufeff':
			continue // 零宽字符
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// 折叠连续空白，去掉行首行尾残留
	out = strings.Join(strings.Fields(out), " ")
	return strings.TrimSpace(out)
}

// isEmoji 粗判表情与装饰符号。分块判断，避免引入大表。
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // 各种 emoji 与补充符号
		return true
	case r >= 0x1F000 && r <= 0x1F2FF: // 麻将/扑克/括号符号
		return true
	case r >= 0x2600 && r <= 0x27BF: // 杂项符号与 dingbats（☀✅✂ 等）
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // 箭头与星形
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // 变体选择符
		return true
	case r == 0x200D: // 零宽连接符（组合 emoji 用）
		return true
	}
	return false
}

// SplitSentences 把一段文本切成适合逐句合成的片段。
//
// 为什么要切：TTS 是非流式的，整段合成意味着要等最后一句才能出声。
// 按句切开后，第一句合成完就能立刻下发，用户感知的延迟就只等于第一句的合成时间。
//
// 规则：
//   - 优先在句末标点后切（。！？；!?;…）
//   - 片段超过 maxRunes 时，退化到在逗号/顿号处切
//   - 仍然超长则硬切，保证单句不会无限长
func SplitSentences(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = 20
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var out []string
	var cur []rune

	flush := func() {
		s := strings.TrimSpace(string(cur))
		if s != "" {
			out = append(out, s)
		}
		cur = cur[:0]
	}

	runes := []rune(text)
	for i, r := range runes {
		cur = append(cur, r)

		if isSentenceEnd(r) {
			// 连续的 ？ ！ 等归到同一句
			if i+1 < len(runes) && isSentenceEnd(runes[i+1]) {
				continue
			}
			// 后面若是右引号/括号，也归入当前句
			if i+1 < len(runes) && isClosingMark(runes[i+1]) {
				continue
			}
			flush()
			continue
		}

		if len(cur) >= maxRunes {
			if idx := lastBreakIndex(cur); idx > 0 {
				head := strings.TrimSpace(string(cur[:idx+1]))
				tail := strings.TrimSpace(string(cur[idx+1:]))
				if head != "" {
					out = append(out, head)
				}
				cur = []rune(tail)
			} else {
				flush()
			}
		}
	}
	flush()
	return out
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '…', '!', '?', ';', '.', '\n':
		return true
	}
	return false
}

func isClosingMark(r rune) bool {
	switch r {
	case '\u300d', '\u300f', '\u201d', '\u2019', '\'', '\uff09', ')', '\u3011', '\u300b':
		return true
	}
	return false
}

func isSoftBreak(r rune) bool {
	switch r {
	case '，', '、', '：', ',', ':':
		return true
	}
	return false
}

// lastBreakIndex 找最后一个可切分的位置（逗号等），没有返回 -1。
func lastBreakIndex(rs []rune) int {
	for i := len(rs) - 1; i > 0; i-- {
		if isSoftBreak(rs[i]) {
			return i
		}
	}
	// 退化：在空白处切
	for i := len(rs) - 1; i > 0; i-- {
		if unicode.IsSpace(rs[i]) {
			return i
		}
	}
	return -1
}
