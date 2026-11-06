package audio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitSyllable(t *testing.T) {
	cases := []struct{ syl, initial, final string }{
		{"xiǎo", "x", "iǎo"},
		{"hēi", "h", "ēi"},
		{"nǐ", "n", "ǐ"},
		{"zhōu", "zh", "ōu"}, // 两字母声母
		{"shí", "sh", "í"},   // 翘舌
		{"wàng", "w", "àng"}, // w 按声母处理（模型就是这么切）
		{"lǜ", "l", "ǜ"},     // 带 ü
		{"ài", "", "ài"},     // 零声母：整音节一个 token
		{"ér", "", "ér"},     // 儿化/自成音节
		{"ān", "", "ān"},
	}
	for _, c := range cases {
		in, fin := splitSyllable(c.syl)
		if in != c.initial || fin != c.final {
			t.Errorf("splitSyllable(%q) = (%q,%q)，期望 (%q,%q)", c.syl, in, fin, c.initial, c.final)
		}
	}
}

// modelTokensPath 返回本仓库里 KWS 模型的 tokens.txt；没下载就跳过。
func modelTokensPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "models",
		"sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01", "tokens.txt")
	if _, err := os.Stat(p); err != nil {
		t.Skip("KWS 模型未下载，跳过（sh deploy/scripts/fetch-models-local.sh）")
	}
	return p
}

func TestBuildKeywordLine_RealTokens(t *testing.T) {
	tokens, err := LoadTokenSet(modelTokensPath(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"小黑":   "x iǎo h ēi",
		"你好小黑": "n ǐ h ǎo x iǎo h ēi",
		"你好军哥": "n ǐ h ǎo j ūn g ē",
		"周望军":  "zh ōu w àng j ūn",
		"女儿":   "n ǚ ér",
	}
	for word, want := range cases {
		line, err := BuildKeywordLine(word, tokens)
		if err != nil {
			t.Fatalf("BuildKeywordLine(%q) 报错: %v", word, err)
		}
		got := strings.TrimSuffix(line, " @"+word)
		if got != want {
			t.Errorf("%q 的 token = %q，期望 %q", word, got, want)
		}
		if !strings.HasSuffix(line, " @"+word) {
			t.Errorf("%q 的行尾缺少 @汉字: %q", word, line)
		}
	}
}

// 模型不认识的读音要报错，而不是悄悄生成一行无效关键词
func TestBuildKeywordLine_UnknownToken(t *testing.T) {
	tokens := map[string]bool{"x": true, "iǎo": true} // 只认识"小"
	if _, err := BuildKeywordLine("小黑", tokens); err == nil {
		t.Fatal("缺 token 时应报错")
	}
}

func TestWriteKeywordsFile_Idempotent(t *testing.T) {
	tokensPath := modelTokensPath(t)
	path := filepath.Join(t.TempDir(), "keywords.txt")

	words, _, err := WriteKeywordsFile(path, []string{"小黑", "你好小黑"}, tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 2 {
		t.Fatalf("应生成 2 个关键词，得到 %v", words)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 同样内容再写一次，文件内容不变（避免每次启动都重写）
	if _, _, err := WriteKeywordsFile(path, []string{"小黑", "你好小黑"}, tokensPath); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("内容未变化时不应改动文件")
	}
	if !strings.Contains(string(first), "@小黑") {
		t.Fatalf("关键词文件内容异常: %s", first)
	}
}
