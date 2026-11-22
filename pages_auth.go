package main

// 运维页面访问控制（/status、/remote）
//
// 这两个页面本身不含私密内容，但 /remote 能往音箱下发播放指令、/status 能
// 看到设备在线情况和缓存目录占用——裸奔在公网上等于把家里的音箱交出去。
// 所以统一要求登录后才能打开：
//
//   - 浏览器：先登录（登录接口现在会写 HttpOnly 的 session_token Cookie），
//     之后直接打开页面即可；
//   - 排障脚本/curl：带 Authorization: Bearer <token> 或 ?token=<token>；
//   - 应急口令：.env 里设 ADMIN_PAGE_TOKEN=xxx 后，可用 /status?key=xxx 直接
//     进去（NAS 上登录坏了也能看状态）。不设这个变量，应急预案完全关闭。

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// sessionCookieName 登录态 Cookie 名，AuthMiddleware 也读它。
const sessionCookieName = "session_token"

// setSessionCookie 登录/注册成功后把 token 写进 HttpOnly Cookie。
// 之前前端只把 token 存 localStorage、后端 Cookie 分支永远是空的，
// 结果「页面级鉴权」这条路走不通（页面导航带不了自定义请求头）。
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	if token == "" {
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie 退出登录时清掉 Cookie，避免「点了退出页面还能进」。
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// pageSessionUser 依次从 Cookie / Authorization / ?token= 里取登录用户名。
func pageSessionUser(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if u := userStore.GetUsernameByToken(c.Value); u != "" {
			return u
		}
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if user, err := userStore.GetUserByToken(strings.TrimPrefix(auth, "Bearer ")); err == nil {
			return user.Username
		}
	}
	if t := strings.TrimSpace(r.URL.Query().Get("token")); t != "" {
		if user, err := userStore.GetUserByToken(t); err == nil {
			return user.Username
		}
	}
	return ""
}

// pageKeyOK 校验应急口令 ADMIN_PAGE_TOKEN（常量时间比较，防时序侧信道）。
func pageKeyOK(r *http.Request) bool {
	want := strings.TrimSpace(os.Getenv("ADMIN_PAGE_TOKEN"))
	if want == "" {
		return false
	}
	got := strings.TrimSpace(r.URL.Query().Get("key"))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// HandleIssueCookie 把「已经登录过、token 存在 localStorage」的浏览器
// 无缝换成 Cookie 登录态，避免打开 /status 时还要再输一次密码。
//
// 注意：这里只认真正的会话 token，不接受 X-Device-Token（设备 token 不能
// 升级成网页登录态）；也不要包 AuthMiddleware，否则设备 token 会顺带通过。
func HandleIssueCookie(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持 POST"})
		return
	}

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		if c, err := r.Cookie(sessionCookieName); err == nil {
			token = c.Value
		}
	}
	if token == "" || userStore.GetUsernameByToken(token) == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "登录已失效"})
		return
	}

	setSessionCookie(w, r, token)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// authOrKey 只读面板接口的鉴权：登录态优先，其次认应急口令 ADMIN_PAGE_TOKEN。
//
// 原来 /status?key=xxx 能打开页面，但页面里的 /api/sources、/api/stats 请求
// 没带登录态，全被 AuthMiddleware 拦成 401 —— 应急入口等于只能看个空壳。
// 这里给「只读」面板接口开一条口令通道，写操作仍然只认登录态。
func authOrKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pageKeyOK(r) {
			next(w, r)
			return
		}
		AuthMiddleware(next)(w, r)
	}
}

// requireLoginPage 包一层页面 handler：未登录返回 401 + 登录框页面。// 用 401 而不是 302，方便脚本一眼看出是「没权限」而不是「页面搬家了」。
func requireLoginPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")

		if pageSessionUser(r) != "" || pageKeyOK(r) {
			next(w, r)
			return
		}

		body, err := os.ReadFile("./theme/gate.html")
		if err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("需要登录后访问。请先打开 / 登录，或使用 ?key=<ADMIN_PAGE_TOKEN>。\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(body)
	}
}
