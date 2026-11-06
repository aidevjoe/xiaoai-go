package main

// 固定应答的合成缓存接线。
//
// 缓存本身在 dialog 包里（ReplyCache），这里负责：
//   - 建缓存目录（数据目录下的 tts-cache，不放模型目录，那儿常是只读挂载）
//   - 启动 / 改配置 / 重载引擎后，把这些"每次都要说"的短句预合成一遍
//
// 刻意**不预生成**：应答文案来自全局设置与档案覆盖，改一次就可能变；
// 预生成只会为用不到的文案白跑合成。首次用到时正常合成并落盘，之后命中缓存。

import (
	"path/filepath"

	"xiaoai-go/internal/server/dialog"
)

// ttsCacheDir 返回缓存目录：与 settings.json 同级的 tts-cache/。
func ttsCacheDir() string {
	dir := filepath.Dir(*flagSettings)
	if dir == "" || dir == "." {
		return "tts-cache"
	}
	return filepath.Join(dir, "tts-cache")
}

// initReplyCache 建缓存并挂到对话框管理器上。
func (s *server) initReplyCache() {
	if s.dm == nil {
		return
	}
	s.dm.Fixed = dialog.NewReplyCache(ttsCacheDir())
}
