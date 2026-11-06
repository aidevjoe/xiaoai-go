package main

// KWS（关键词唤醒）的关键词文件准备。
//
// sherpa 的 keyword spotter 在构造时就把 keywords.txt 读进去，所以：
//   - 后台改唤醒词 → 必须重建引擎才生效（前端据此提示"需要重载"）
//   - 关键词文件不能放在模型目录（那里通常是只读挂载），统一写到数据目录
//
// 关键词内容由唤醒词自动生成（汉字→带声调拼音→声母/韵母 token），
// 见 internal/server/audio/kwstext.go。

import (
	"fmt"
	"path/filepath"

	"xiaoai-go/internal/server/audio"
)

// prepareKWSKeywords 按当前唤醒词生成 keywords.txt，返回文件路径与实际生效的关键词。
// 返回的 []error 是"某个唤醒词转不出来"这类局部问题，不影响其它词。
func (s *server) prepareKWSKeywords(cfg audio.SherpaConfig) (path string, words []string, errs []error, err error) {
	if cfg.KWSTokens == "" {
		return "", nil, nil, fmt.Errorf("模型目录 %s 下没找到 KWS 模型（需要含 encoder*.onnx 与 tokens.txt 的子目录）", cfg.ModelDir)
	}
	reg := s.reg.Load()
	if reg == nil {
		return "", nil, nil, fmt.Errorf("助手档案还没初始化，拿不到唤醒词")
	}
	wake := reg.WakeWords()
	if len(wake) == 0 {
		return "", nil, nil, fmt.Errorf("没有配置任何唤醒词")
	}
	path = s.kwsKeywordsPath()
	built, errs, err := audio.WriteKeywordsFile(path, wake, cfg.KWSTokens)
	if err != nil {
		return "", nil, errs, err
	}
	return path, built, errs, nil
}

// kwsKeywordsPath 关键词文件落在数据目录（与 settings.json 同级），不放模型目录。
func (s *server) kwsKeywordsPath() string {
	dir := filepath.Dir(*flagSettings)
	if dir == "" || dir == "." {
		return "keywords.txt"
	}
	return filepath.Join(dir, "keywords.txt")
}
