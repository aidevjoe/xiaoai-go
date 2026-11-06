package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xiaoai-go/internal/proto"
)

func TestWriteKeywordsCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "wakewords.txt")

	res, err := writeKeywords(proto.KWSSyncPayload{
		Words: []string{"小黑", "小智"},
		Rev:   "abcdef123456",
	}, path)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !res.OK || res.Count != 2 || res.Skip {
		t.Fatalf("返回不对: %+v", res)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "rev=abcdef123456") {
		t.Fatalf("缺少 rev 标记: %s", body)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if lines[len(lines)-2] != "小黑" || lines[len(lines)-1] != "小智" {
		t.Fatalf("词写错了: %#v", lines)
	}
	// 不应留下临时文件
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("残留 .tmp")
	}
}

// 同一版本重复下发不应该每次都写 flash。
func TestWriteKeywordsSkipsSameRev(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wakewords.txt")
	p := proto.KWSSyncPayload{Words: []string{"小黑"}, Rev: "v1"}

	if _, err := writeKeywords(p, path); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(path)

	res, err := writeKeywords(p, path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skip {
		t.Fatalf("同版本应跳过写入: %+v", res)
	}
	second, _ := os.Stat(path)
	if !first.ModTime().Equal(second.ModTime()) {
		t.Fatal("文件被无谓地重写了")
	}

	// 版本变了必须重写
	p.Rev = "v2"
	res, err = writeKeywords(p, path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skip {
		t.Fatal("版本变化后必须重写")
	}
}

// 负载里带路径时以负载为准（服务端可指定目录）。
func TestWriteKeywordsUsesPayloadPath(t *testing.T) {
	def := filepath.Join(t.TempDir(), "default.txt")
	custom := filepath.Join(t.TempDir(), "custom", "kw.txt")

	res, err := writeKeywords(proto.KWSSyncPayload{Words: []string{"小黑"}, Rev: "r"}, custom)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != custom {
		t.Fatalf("应使用指定路径: %s", res.Path)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatal("指定路径没写出来")
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Fatal("不该写默认路径")
	}
}

// 空负载不该把已有文件清空。
func TestWriteKeywordsRejectsEmptyPath(t *testing.T) {
	if _, err := writeKeywords(proto.KWSSyncPayload{Words: []string{"a"}}, ""); err == nil {
		t.Fatal("没有路径应报错")
	}
}

func TestRevOf(t *testing.T) {
	cases := map[string]string{
		"# xiaoai-go wakewords v1 rev=abc123 updated=2026\n小黑\n": "abc123",
		"no rev here": "",
		"rev=x\r\n小黑": "x",
	}
	for in, want := range cases {
		if got := revOf(in); got != want {
			t.Errorf("revOf(%q) = %q，期望 %q", in, got, want)
		}
	}
}
