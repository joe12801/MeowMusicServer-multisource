package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 本文件收敛「对外暴露面」相关的公共能力：
//   - 原子写文件（防止断电/并发写坏 JSON）
//   - 统一中间件（panic 恢复 + 安全响应头）
//   - 简易限流（登录/注册防爆破）
//   - 远程代理的 SSRF 防护（域名白名单 + 解析结果禁止内网）

func fmtLogf(format string, args ...interface{}) {
	fmt.Printf("[Security] "+format+"\n", args...)
}

// ---------- 原子写文件 ----------

// writeFileAtomic 先写临时文件、fsync、再 rename 覆盖。
// 直接 os.WriteFile 到目标路径时，进程被杀或断电会留下半截 JSON，
// 下次启动 loadFromFile 直接解析失败 → 所有用户/歌单丢失。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		// 单文件 bind mount（docker -v host/devices.json:/app/devices.json）上
		// rename 到挂载点会返回 EBUSY —— 挂载点不能被替换。
		// 这时退化成「原地覆盖 + fsync」，牺牲一点原子性，否则设备绑定
		// 只在内存里生效，容器一重启就全丢（实测踩过）。
		if ferr := writeInPlace(path, data, perm); ferr != nil {
			return fmt.Errorf("rename: %v; fallback write: %v", err, ferr)
		}
		os.Remove(tmpName)
		tmpName = ""
		return nil
	}
	tmpName = ""
	return nil
}

// writeInPlace 直接覆盖目标文件（用于不能 rename 的挂载点）。
func writeInPlace(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ---------- 统一中间件 ----------

// handle 注册路由，并统一包上 panic 恢复与安全响应头。
func handle(pattern string, h http.HandlerFunc) {
	http.Handle(pattern, withCommonMiddleware(h))
}

func withCommonMiddleware(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		defer func() {
			if rec := recover(); rec != nil {
				fmtLogf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		h(w, r)
	})
}

// serveThemeFile 返回 theme 目录下的指定页面；文件不存在就 404，
// 不做目录列举，避免把 theme 目录里的其它文件暴露出去。
func serveThemeFile(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join("theme", filepath.Base(name))
		if _, err := os.Stat(p); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, p)
	}
}

// ---------- 限流 ----------

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: make(map[string][]time.Time), limit: limit, window: window}
}

func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	cutoff := now.Add(-rl.window)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	list := rl.hits[key]
	i := 0
	for ; i < len(list); i++ {
		if list[i].After(cutoff) {
			break
		}
	}
	list = list[i:]

	// 简单的容量保护：条目太多就整体丢弃过期数据
	if len(rl.hits) > 5000 {
		for k, v := range rl.hits {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(rl.hits, k)
			}
		}
	}

	if len(list) >= rl.limit {
		rl.hits[key] = list
		return false
	}
	rl.hits[key] = append(list, now)
	return true
}

// clientIP 取请求来源 IP（不信任 X-Forwarded-For，避免被伪造绕过限流）
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limitByIP 按来源 IP 限流，超出返回 429
func limitByIP(rl *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests, please retry later", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// ---------- 远程代理 / SSRF 防护 ----------

// 默认允许代理的域名后缀（音源 CDN）。可用 PROXY_ALLOWED_HOSTS 追加，
// 设为 "*" 则关闭域名白名单（仅保留内网地址拦截）。
var defaultProxyHosts = []string{
	"kuwo.cn", "kuwo.com", "kwcdn.kuwo.cn",
	"126.net", "127.net", "music.163.com",
	"qq.com", "qqmusic.qq.com",
	"kugou.com", "kgimg.com", "kglink.com",
	"xfabe.com", "gdstudio.xyz", "994938.xyz",
	"yaohud.cn", "yuafeng.cn", "migu.cn",
	"googlevideo.com", "ytimg.com", "youtube.com",
	"hdslb.com", "bilivideo.com", "bilivideo.cn", "akamaized.net",
	// 荔枝 FM：音频在 cdn5/cdn101.lizhi.fm，封面在 cdnimg*.gzlzfm.com
	"lizhi.fm", "gzlzfm.com",
}

func proxyHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}

	allowAll := false
	extra := strings.TrimSpace(os.Getenv("PROXY_ALLOWED_HOSTS"))
	if extra == "*" {
		allowAll = true
	}

	check := func(suffix string) bool {
		suffix = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(suffix), "."))
		if suffix == "" {
			return false
		}
		return host == suffix || strings.HasSuffix(host, "."+suffix)
	}

	if allowAll {
		return true
	}
	for _, s := range defaultProxyHosts {
		if check(s) {
			return true
		}
	}
	if extra != "" {
		for _, s := range strings.Split(extra, ",") {
			if check(s) {
				return true
			}
		}
	}
	return false
}

// isBlockedIP 判断是否为不该被代理访问的地址（内网/保留/多播等）
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0:
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // CGNAT 100.64/10
			return true
		case ip4[0] == 169 && ip4[1] == 254: // 云元数据
			return true
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0: // 192.0.0.0/24
			return true
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 基准测试网段
			return true
		case ip4[0] == 192 && ip4[1] == 88 && ip4[2] == 99: // 6to4 中继
			return true
		}
	}
	return false
}

// safeDialControl 在真正建立连接前检查解析出的地址，
// 这样即使域名白名单里的域名被解析到内网（DNS rebinding）也连不出去。
func safeDialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address: %s", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || isBlockedIP(ip) {
		return fmt.Errorf("blocked address: %s", address)
	}
	return nil
}

var proxyHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   safeDialControl,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		if !proxyHostAllowed(req.URL.Hostname()) {
			return fmt.Errorf("redirect host not allowed: %s", req.URL.Hostname())
		}
		return nil
	},
}
