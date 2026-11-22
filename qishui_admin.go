package main

// 汽水音乐：状态与扫码登录（/tools 页面用）
//
// 汽水的搜索和完整播放都要求登录态，所以必须让用户扫一次码：
//
//	POST /api/v1/system/qr_login/soda        生成二维码（返回 data:image/png;base64）
//	GET  /api/v1/system/qr_login/soda?key=.. 轮询状态，成功后 sidecar 自己把
//	                                         cookie 写进 cookies.json
//
// cookies.json 在 compose 里挂在宿主机 data/qishui/cookies.json，
// 所以容器重建也不用重新扫（除非 cookie 过期）。
//
// 本站接口（都要登录，站长在 /tools 里操作）：
//   GET  /api/qishui/status      {enabled, reachable, logged_in, cookie, sources}
//   POST /api/qishui/qr          {key, image, url}
//   GET  /api/qishui/qr?key=..   {status, message}

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func qishuiHTTP(method, path string) ([]byte, error) {
	req, err := http.NewRequest(method, qishuiBase()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0")
	resp, err := qishuiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("上游 HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// qishuiCookieState 读 sidecar 的当前 cookie，判断汽水是否已经登录。
func qishuiCookieState() (loggedIn bool, masked string) {
	body, err := qishuiHTTP("GET", "/api/v1/system/cookies")
	if err != nil {
		return false, ""
	}
	var wrap struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(body, &wrap) != nil {
		return false, ""
	}
	// data 里直接是 {source: cookie}；也兼容 {cookies:{...}} 的旧格式
	c := wrap.Data
	if inner, ok := wrap.Data["cookies"]; ok && strings.HasPrefix(strings.TrimSpace(inner), "{") {
		var m map[string]string
		if json.Unmarshal([]byte(inner), &m) == nil {
			c = m
		}
	}
	raw := strings.TrimSpace(c["soda"])
	if raw == "" {
		return false, ""
	}
	return true, maskSecret(raw)
}

// 汽水可用性探测。
//
// 实测结论（2026-11，服务器出口在东京 Oracle）：
//   - 榜单 /api/v1/ranking/hot 是公开的，境外 IP 也能读；字节风控偶尔抖一下
//     会回 ERR_REQUEST_FORBIDDEN，过一会儿又恢复；
//   - 搜索 /api/v1/music/search 必须带登录态，未登录恒返回 songs:null，
//     和 IP 无关；
//   - 扫码登录能走到 "scanned"（手机确实扫了），但服务器换 ticket 那步
//     拿不到 "confirmed"，所以一直登不上。
//
// 于是给前端两个结论：
//   usable     链路活着（榜单能读），可以放「汽水」这个筛选项
//   searchable 能搜全库（需要登录）；false 时只覆盖榜单里的歌
type qishuiProbeResult struct {
	usable     bool
	searchable bool
	reason     string
}

var (
	qishuiProbeMu  sync.Mutex
	qishuiProbeAt  time.Time
	qishuiProbeRes qishuiProbeResult
)

const qishuiProbeTTL = 10 * time.Minute

func qishuiProbe() qishuiProbeResult {
	if !qishuiEnabled() {
		return qishuiProbeResult{reason: "汽水源已关闭（QISHUI_ENABLED=0）"}
	}
	qishuiProbeMu.Lock()
	defer qishuiProbeMu.Unlock()
	if time.Since(qishuiProbeAt) < qishuiProbeTTL {
		return qishuiProbeRes
	}

	res := qishuiProbeResult{}
	body, err := qishuiHTTP("GET", "/api/v1/ranking/hot?source=soda&limit=1")
	switch {
	case err != nil:
		res.reason = "连不上汽水服务（gomusic 容器没跑？）"
	default:
		var wrap struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(body, &wrap)
		switch {
		case wrap.Code == 200:
			res.usable = true
		case strings.Contains(wrap.Msg, "FORBIDDEN") || strings.Contains(wrap.Msg, "1000006"):
			res.reason = "汽水风控拦住了本机（境外 IP 常见），连榜单都读不到"
		default:
			res.reason = "汽水接口异常"
		}
	}

	if res.usable {
		sb, serr := qishuiHTTP("GET", "/api/v1/music/search?q="+url.QueryEscape("晴天")+"&type=song&sources=soda&limit=1")
		if serr == nil {
			var sw struct {
				Data struct {
					Songs []qishuiSong `json:"songs"`
				} `json:"data"`
			}
			if json.Unmarshal(sb, &sw) == nil && len(sw.Data.Songs) > 0 {
				res.searchable = true
			}
		}
	}

	qishuiProbeAt, qishuiProbeRes = time.Now(), res
	return res
}

// HandleQishuiAdmin /api/qishui/status | /api/qishui/qr
func HandleQishuiAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	path := strings.TrimPrefix(r.URL.Path, "/api/qishui")
	switch {
	case path == "/status" || path == "":
		logged, masked := qishuiCookieState()
		ok := true
		if _, err := qishuiHTTP("GET", "/api/v1/system/cookies"); err != nil {
			ok = false
		}
		p := qishuiProbe()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":    qishuiEnabled(),
			"base":       qishuiBase(),
			"reachable":  ok,
			"logged_in":  logged,
			"cookie":     masked,
			"usable":     p.usable,
			"searchable": p.searchable,
			"reason":     p.reason,
		})
		return

	case path == "/qr":
		if r.Method == http.MethodPost {
			body, err := qishuiHTTP("POST", "/api/v1/system/qr_login/soda")
			if err != nil {
				http.Error(w, "生成二维码失败："+err.Error(), http.StatusBadGateway)
				return
			}
			var wrap struct {
				Data struct {
					Key      string `json:"key"`
					URL      string `json:"url"`
					ImageURL string `json:"image_url"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &wrap) != nil || wrap.Data.Key == "" {
				http.Error(w, "二维码响应异常", http.StatusBadGateway)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"key":   wrap.Data.Key,
				"image": wrap.Data.ImageURL,
				"url":   wrap.Data.URL,
			})
			return
		}

		// GET：轮询
		key := strings.TrimSpace(r.URL.Query().Get("key"))
		if key == "" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		body, err := qishuiHTTP("GET", "/api/v1/system/qr_login/soda?key="+url.QueryEscape(key))
		if err != nil {
			http.Error(w, "查询失败："+err.Error(), http.StatusBadGateway)
			return
		}
		var wrap struct {
			Data struct {
				Status  interface{} `json:"status"`
				Message string      `json:"message"`
			} `json:"data"`
			Msg string `json:"msg"`
		}
		json.Unmarshal(body, &wrap)
		status := ""
		switch v := wrap.Data.Status.(type) {
		case string:
			status = v
		case float64:
			status = strconv.Itoa(int(v))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  status,
			"message": wrap.Data.Message,
			"raw":     wrap.Msg,
			"at":      time.Now().Unix(),
		})
		return
	}

	http.NotFound(w, r)
}
