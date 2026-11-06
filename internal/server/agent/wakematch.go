package agent

// 用 ASR 文本做唤醒。
//
// 为什么需要：服务端没有 KWS 模型、音箱端也没跑本地 KWS，喊唤醒词没人理。
// 与其做一个"任何话都回答"的免唤醒模式（家里聊天全被当指令，且没法测真实流程），
// 不如把唤醒判定挪到识别之后：识别文本里出现唤醒词才进入对话，
// 效果和"喊一声 → 应答 → 提问 → 回答"完全一致，只是多花一次 ASR 的时间（~150ms）。
//
// 匹配要容错：实测「小黑」会被识别成「小飞」/「小嘿」。所以除了子串匹配，
// 还做滑窗编辑距离比较——两个字错一个仍算命中。

import "strings"

// MatchWake 判断文本里是否包含某个唤醒词，返回命中的那个。
//
// 容错规则：
//   - 归一化后直接包含（最常见）
//   - 否则逐窗口算编辑距离：2~3 个字容忍 1 个错字，更长的容忍 2 个
//
// 不追求零误唤醒——喊错一两次比"喊了没反应"体验好得多。
func MatchWake(text string, words []string) (string, bool) {
	nt := normalizeWakeText(text)
	if nt == "" {
		return "", false
	}
	for _, w := range words {
		nw := normalizeWakeText(w)
		if nw == "" {
			continue
		}
		if strings.Contains(nt, nw) {
			return w, true
		}
	}
	// 子串都没中，再做容错比较
	for _, w := range words {
		nw := []rune(normalizeWakeText(w))
		if len(nw) < 2 {
			continue
		}
		tol := toleranceFor(nw)
		tr := []rune(nt)
		for i := 0; i+len(nw) <= len(tr); i++ {
			if editDistance(tr[i:i+len(nw)], nw) <= tol {
				return w, true
			}
		}
	}
	return "", false
}

// MatchWakeEx 与 MatchWake 相同，但额外返回"唤醒词后面那句话"。
//
// 真实说法经常是一口气说完：「你好小黑，现在几点」。这时候不该把整句丢掉
// 让用户重说一遍，而是把唤醒词剥掉、剩下的当提问直接回答。
func MatchWakeEx(text string, words []string) (keyword, rest string, ok bool) {
	nt := normalizeWakeText(text)
	if nt == "" {
		return "", "", false
	}
	tr := []rune(nt)
	for _, w := range words {
		nw := []rune(normalizeWakeText(w))
		if len(nw) == 0 {
			continue
		}
		// 先找精确子串
		if i := indexRunes(tr, nw); i >= 0 {
			return w, trimGreeting(tr[:i], tr[i+len(nw):]), true
		}
	}
	// 容错匹配：命中窗口就从原文里扣掉
	for _, w := range words {
		nw := []rune(normalizeWakeText(w))
		if len(nw) < 2 {
			continue
		}
		tol := toleranceFor(nw)
		for i := 0; i+len(nw) <= len(tr); i++ {
			if editDistance(tr[i:i+len(nw)], nw) <= tol {
				return w, trimGreeting(tr[:i], tr[i+len(nw):]), true
			}
		}
	}
	return "", "", false
}

// toleranceFor 决定一个唤醒词允许错几个字。
//
// 两个字一律不容错：2 字容 1 错等于"认对一半就算"，
// 实测喊「小爱同学」被识别成「小同学」，滑窗里"小同"与"小黑"距离 1，
// 直接把小黑唤醒了。宁可让用户多喊一遍，也不能天天被别的词误唤醒。
func toleranceFor(word []rune) int {
	switch {
	case len(word) <= 2:
		return 0
	case len(word) == 3:
		return 1
	default:
		return 2
	}
}

// indexRunes 在 rs 里找 sub 的位置，找不到返回 -1。
func indexRunes(rs, sub []rune) int {
	if len(sub) == 0 || len(rs) < len(sub) {
		return -1
	}
	for i := 0; i+len(sub) <= len(rs); i++ {
		hit := true
		for j := range sub {
			if rs[i+j] != sub[j] {
				hit = false
				break
			}
		}
		if hit {
			return i
		}
	}
	return -1
}

// trimGreeting 去掉唤醒词前后的语气词，剩下的太短就当没有提问。
func trimGreeting(before, after []rune) string {
	// 唤醒词前面通常只有"你好/喂/嗨/嗯"，一并扔掉
	rest := string(after)
	for _, g := range []string{"你好", "您好", "喂", "嗨", "哎", "嗯", "那个"} {
		rest = strings.TrimPrefix(rest, g)
	}
	rest = strings.TrimSpace(rest)
	// 一个字不算提问（"嗯""啊"这类），两个字起才当问题
	if len([]rune(rest)) < 2 {
		return ""
	}
	_ = before
	return rest
}

// normalizeWakeText 去掉空白与标点，只留内容字符。
func normalizeWakeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '。', '，', '！', '？', '、', '.', ',', '!', '?', '：', ':', '…':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// editDistance 是标准的莱文斯坦距离（逐字符插入/删除/替换）。
// 只用于很短的串（≤6 字），不做优化也够快。
func editDistance(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// WakeWords 返回全部档案的唤醒词（去重），供 ASR 唤醒匹配用。
func (r *Registry) WakeWords() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]bool, len(r.profiles)*2)
	out := make([]string, 0, len(r.profiles)*2)
	for _, p := range r.profiles {
		for _, w := range p.WakeWords {
			w = strings.TrimSpace(w)
			if w == "" || seen[w] {
				continue
			}
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}
