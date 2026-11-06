// Package web 内嵌管理后台前端产物（Vite 构建后落到 dist/）。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler 返回静态资源处理器。前端用相对路径（base './'），可直接挂在根路由。
//
// 必须自己设缓存头：
// index.html 引用的是带 hash 的产物名，但浏览器默认的启发式缓存会把 index.html
// 本身也缓存住。于是前端重新构建后，手机/浏览器还在跑旧页面——
// 表现成"改了样式没生效"，很难判断到底是没改对还是没加载到。
//
//   - index.html 等入口：每次回源（no-store，它只有 400 字节）
//   - /assets/*：文件名带内容 hash，可以长期强缓存
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServerFS(sub)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			// 内容变了文件名就会变，放心让浏览器用本地副本
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// 入口页（含 SPA 的 hash 路由根路径）一律不缓存
			w.Header().Set("Cache-Control", "no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		fileServer.ServeHTTP(w, r)
	})
}
