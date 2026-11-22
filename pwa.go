package main

// PWA 静态资源：manifest、Service Worker、图标
//
// 这三样都不能走 serveThemeFile（它固定 text/html），否则浏览器会拒绝
// 解析 manifest、也会把 sw.js 当成 HTML 拒绝注册。

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func serveThemeAsset(name, ctype, cacheControl string, extra map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join("theme", filepath.Base(name))
		if _, err := os.Stat(p); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		for k, v := range extra {
			w.Header().Set(k, v)
		}
		http.ServeFile(w, r, p)
	}
}

// HandleManifest 提供 /manifest.webmanifest。
func HandleManifest() http.HandlerFunc {
	return serveThemeAsset("manifest.webmanifest",
		"application/manifest+json; charset=utf-8", "public, max-age=3600", nil)
}

// HandleServiceWorker 提供 /sw.js。
//
// no-cache 是必须的：Service Worker 文件如果被浏览器长期缓存，更新脚本
// 后用户永远拿不到新版本。Service-Worker-Allowed 允许它作用到整站。
func HandleServiceWorker() http.HandlerFunc {
	return serveThemeAsset("sw.js",
		"application/javascript; charset=utf-8", "no-cache",
		map[string]string{"Service-Worker-Allowed": "/"})
}

// HandleIcons 提供 /icons/*.png，只允许 theme/icons 下的 png 文件。
func HandleIcons(w http.ResponseWriter, r *http.Request) {
	name := path.Base(strings.TrimPrefix(r.URL.Path, "/icons/"))
	if name == "" || name == "." || name == "/" || !strings.HasSuffix(strings.ToLower(name), ".png") {
		http.NotFound(w, r)
		return
	}
	// 只接受我们自己生成的那几个文件名，别的一律 404，避免被当作任意文件读取
	switch name {
	case "icon-192.png", "icon-512.png", "icon-maskable-512.png",
		"apple-touch-icon.png", "favicon-32.png":
	default:
		http.NotFound(w, r)
		return
	}
	p := filepath.Join("theme", "icons", name)
	if _, err := os.Stat(p); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	http.ServeFile(w, r, p)
}
