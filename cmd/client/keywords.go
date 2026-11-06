package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xiaoai-go/internal/proto"
)

// writeKeywords 把服务端下发的唤醒词清单写到本地文件。
//
// 几个刻意的选择：
//   - 原子写（tmp + rename）：写到一半断电也不会留下半截文件
//   - 版本去重：文件首行带 rev，版本一致直接返回 skip，避免每次心跳都写 flash
//   - 不碰 sherpa 的 keywords.txt：那个文件要 token 化，覆盖了会让 KWS 加载失败；
//     这里用独立的 wakewords.txt，路径可在服务端配置
func writeKeywords(p proto.KWSSyncPayload, defPath string) (*proto.KWSSyncResult, error) {
	path := strings.TrimSpace(p.Path)
	if path == "" {
		path = defPath
	}
	if path == "" {
		return nil, errors.New("没有指定唤醒词文件路径")
	}

	// 版本一致就不动磁盘
	if p.Rev != "" {
		if old, err := os.ReadFile(path); err == nil {
			if revOf(string(old)) == p.Rev {
				return &proto.KWSSyncResult{
					OK: true, Path: path, Count: len(p.Words),
					Rev: p.Rev, Skip: true, Bytes: len(old),
					Message: "版本一致，未重写",
				}, nil
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("建目录失败: %w", err)
	}

	var b strings.Builder
	b.WriteString("# xiaoai-go wakewords v1 rev=")
	b.WriteString(p.Rev)
	b.WriteString(" updated=")
	b.WriteString(time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString("\n# 由服务端下发，请勿手工编辑；改唤醒词请到管理后台的「助手档案」\n")
	n := 0
	for _, w := range p.Words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		b.WriteString(w)
		b.WriteByte('\n')
		n++
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return nil, fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("替换文件失败: %w", err)
	}

	return &proto.KWSSyncResult{
		OK: true, Path: path, Count: n, Rev: p.Rev, Bytes: b.Len(),
	}, nil
}

// revOf 从文件内容里取出 rev=N 里的 N。
func revOf(content string) string {
	const tag = "rev="
	line := content
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	i := strings.Index(line, tag)
	if i < 0 {
		return ""
	}
	rest := line[i+len(tag):]
	if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
