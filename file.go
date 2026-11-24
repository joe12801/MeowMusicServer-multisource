package main

import (
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ListFiles function: Traverse all files in the specified directory and return a slice of the file path
func ListFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// GetFileContent function: Read the content of a specified file and return it
func GetFileContent(filePath string) ([]byte, error) {
	return os.ReadFile(filePath)
}

// fileHandler 是除业务路由之外的统一入口，只做两件事：
//  1. /url/ 前缀 → 受限的远程资源代理（封面/音频转发）
//  2. 其它 → ./files 下的静态文件，且必须经过白名单校验
func fileHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "MeowMusicEmbeddedServer")

	if strings.HasPrefix(r.URL.Path, "/url/") {
		proxyRemoteHandler(w, r)
		return
	}
	serveLocalFile(w, r)
}

// 允许匿名访问的顶层静态文件扩展名。
// 顶层只放背景图/favicon 这类资源，数据文件（*.json）永远不在其中。
var publicTopLevelExt = map[string]bool{
	".webp": true, ".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".bmp": true, ".svg": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".css": true,
}

// 允许匿名访问的子目录（音频缓存与本地音乐库）
var publicSubDirs = []string{"cache", "music"}

// serveLocalFile 把 ./files 下的文件返回给客户端。
//
// 旧实现直接 filepath.Join("./files", r.URL.Path) 并且对已经解码过的
// 路径又做了一次 QueryUnescape，等于把整个数据目录（users.json、
// sessions.json、user_playlists.json）暴露成静态资源，还能用双编码
// 绕过 ServeMux 的 ".." 重定向去读 /etc/passwd。
// 现在改为：解码一次（交给 net/http）+ 白名单目录 + 最终路径包含性校验 +
// 拒绝 .json/隐藏文件。
func serveLocalFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// r.URL.Path 已经是解码过一次的路径，绝不能再解第二次。
	p := r.URL.Path
	if strings.ContainsRune(p, 0) || strings.Contains(p, "..") {
		NotFoundHandler(w, r)
		return
	}

	// 兼容历史数据：旧版本用 url.QueryEscape 生成链接，空格会变成 "+"
	p = strings.ReplaceAll(p, "+", " ")

	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" {
		NotFoundHandler(w, r)
		return
	}

	// 白名单：要么是允许的顶层静态文件，要么在 cache/music 子目录里
	parts := strings.Split(rel, "/")
	for _, seg := range parts {
		if seg == "" || strings.HasPrefix(seg, ".") {
			NotFoundHandler(w, r)
			return
		}
	}
	ext := strings.ToLower(filepath.Ext(rel))
	if ext == ".json" || ext == ".go" || ext == ".env" {
		NotFoundHandler(w, r)
		return
	}
	if len(parts) == 1 {
		if !publicTopLevelExt[ext] {
			NotFoundHandler(w, r)
			return
		}
	} else {
		allowed := false
		for _, d := range publicSubDirs {
			if parts[0] == d {
				allowed = true
				break
			}
		}
		if !allowed {
			NotFoundHandler(w, r)
			return
		}
	}

	fullPath := filepath.Join("./files", filepath.FromSlash(rel))

	// 最终防线：解析成绝对路径后必须仍在 ./files 内（防符号链接/边界绕过）
	root, err := filepath.Abs("./files")
	if err != nil {
		NotFoundHandler(w, r)
		return
	}
	absPath, err := filepath.Abs(fullPath)
	if err != nil || absPath != root && !strings.HasPrefix(absPath, root+string(filepath.Separator)) {
		NotFoundHandler(w, r)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		NotFoundHandler(w, r)
		return
	}
	// 空的 music.mp3 视为没有缓存，交回上层走转码链路
	if info.Size() == 0 && strings.HasSuffix(clean, "/music.mp3") {
		NotFoundHandler(w, r)
		return
	}

	f, err := os.Open(absPath)
	if err != nil {
		NotFoundHandler(w, r)
		return
	}
	defer f.Close()

	setContentType(w, absPath)
	if parts[0] == "cache" {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	// 音频文件：只要不是「从中间续读」，一律 200 定长整包发完。
	// 固件把 Read() 返回 0 当成下载结束，206 分段响应会让它提前停
	// （播一会就没声、一卡一卡）。这一分支与 v1.6.6 之前的 fileHandler 一致；
	// 浏览器拖进度条时发的是 bytes=N-（N>0），仍走 ServeContent 的 206。
	if parts[0] == "cache" && isAudioFile(absPath) && !wantsMidStreamRange(r) {
		if err := serveAudioFull(w, r, f, info.Size()); err != nil {
			fmtLogf("[Static] audio send error: %v", err)
		}
		return
	}

	// ServeContent 自带 Range / If-Modified-Since 支持，浏览器拖进度条、
	// 断点续传都不用重下整首。
	http.ServeContent(w, r, filepath.Base(absPath), info.ModTime(), f)
}

// isAudioFile 判断扩展名是不是设备会直接播放的音频。
func isAudioFile(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".mp3", ".wav", ".flac", ".aac", ".ogg", ".m4a", ".amr":
		return true
	}
	return false
}

// proxyRemoteHandler 只为「音源给的远程封面 / 音频链接」做转发。
//
// 旧实现是一个无鉴权、无白名单、无体积上限的开放代理：任何人都能用它
// 探测内网（127.0.0.1、169.254.169.254、NAS 管理口），或者拿一个大文件
// 把内存打满（io.ReadAll 全读进内存）。
// 现在：域名白名单 + 解析后禁止内网/保留地址 + 流式转发 + 体积上限。
const maxProxyBytes = 64 << 20 // 64MB，够一首无损，也够防内存打满

func proxyRemoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	raw := strings.TrimPrefix(r.URL.Path, "/url/")
	var scheme string
	switch {
	case strings.HasPrefix(raw, "http/"):
		scheme, raw = "http://", raw[len("http/"):]
	case strings.HasPrefix(raw, "https/"):
		scheme, raw = "https://", raw[len("https/"):]
	default:
		NotFoundHandler(w, r)
		return
	}

	target, err := url.Parse(scheme + raw)
	if err != nil || target.Hostname() == "" || target.User != nil {
		NotFoundHandler(w, r)
		return
	}
	if !proxyHostAllowed(target.Hostname()) {
		fmtLogf("[Proxy] blocked host: %s", target.Hostname())
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		NotFoundHandler(w, r)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0")

	resp, err := proxyHTTPClient.Do(req)
	if err != nil {
		fmtLogf("[Proxy] request failed: %v", err)
		NotFoundHandler(w, r)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		NotFoundHandler(w, r)
		return
	}

	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		setContentType(w, target.Path)
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")

	// 声明长度时不能超上限，未声明长度则用 LimitReader 兜住
	if resp.ContentLength > maxProxyBytes {
		http.Error(w, "response too large", http.StatusBadGateway)
		return
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, maxProxyBytes))
}

func setContentType(w http.ResponseWriter, p string) {
	ext := strings.ToLower(filepath.Ext(p))
	contentTypes := map[string]string{
		".mp3":  "audio/mpeg",
		".wav":  "audio/wav",
		".flac": "audio/flac",
		".aac":  "audio/aac",
		".ogg":  "audio/ogg",
		".m4a":  "audio/mp4",
		".amr":  "audio/amr",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".bmp":  "image/bmp",
		".svg":  "image/svg+xml",
		".webp": "image/webp",
		".ico":  "image/x-icon",
		".txt":  "text/plain; charset=utf-8",
		".lrc":  "text/plain; charset=utf-8",
		".mrc":  "text/plain; charset=utf-8",
		".json": "application/json",
	}
	if ct, ok := contentTypes[ext]; ok {
		w.Header().Set("Content-Type", ct)
		return
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
}
