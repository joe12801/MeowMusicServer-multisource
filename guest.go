package main

// 游客试听额度
//
// 服务挂在公网上，任何人拿到地址都能听——不登录也能点歌，意味着别人可以把
// 你的 NAS 当免费音乐站用，还会把转码额度吃光。
//
// 做法：未登录（既没有会话 Cookie、也没有设备 Token）的请求按来源 IP 计数，
// 每天最多听 GUEST_DAILY_SONGS 首（默认 20），超了返回 429 + 提示去登录。
// 登录用户和已绑定设备不受影响。
//
// 计数只在「一次播放的第一次请求」上加：<audio> 拿 Range: bytes=0- 才开始，
// 之后拖进度条、断线续传的 Range 不算，避免一首歌被算成五六首。
//
// GET /api/guest/quota  → 前端提前判断，额度用尽就别发起播放，直接弹登录框。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	guestFilePath = "./files/guest.json"
	guestSaveGap  = 10 * time.Second
)

type guestStore struct {
	mu       sync.Mutex
	Day      string         `json:"day"`
	IPs      map[string]int `json:"ips"`
	lastSave time.Time
	dirty    bool
}

var guestQuota = &guestStore{IPs: map[string]int{}}

// guestDailyLimit 0 = 不限（等于关掉游客限制）。
func guestDailyLimit() int {
	n := envInt("GUEST_DAILY_SONGS", 20)
	if n < 0 {
		return 0
	}
	return n
}

func initGuestStore() {
	if data, err := os.ReadFile(guestFilePath); err == nil {
		var loaded guestStore
		if json.Unmarshal(data, &loaded) == nil && loaded.IPs != nil {
			guestQuota.Day = loaded.Day
			guestQuota.IPs = loaded.IPs
		}
	}
	if guestQuota.IPs == nil {
		guestQuota.IPs = map[string]int{}
	}
	if guestQuota.Day != todayKey() {
		guestQuota.IPs = map[string]int{}
		guestQuota.Day = todayKey()
	}
	if n := guestDailyLimit(); n > 0 {
		fmt.Printf("[Guest] 游客每天可试听 %d 首（GUEST_DAILY_SONGS，0=不限）\n", n)
	} else {
		fmt.Println("[Guest] 游客试听不限（GUEST_DAILY_SONGS=0）")
	}
}

func (g *guestStore) enterLocked() {
	if g.Day != todayKey() {
		g.Day = todayKey()
		g.IPs = map[string]int{}
		g.dirty = true
	}
}

func (g *guestStore) saveLocked(force bool) {
	if !g.dirty {
		return
	}
	if !force && time.Since(g.lastSave) < guestSaveGap {
		return
	}
	data, err := json.MarshalIndent(struct {
		Day string         `json:"day"`
		IPs map[string]int `json:"ips"`
	}{g.Day, g.IPs}, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(guestFilePath, data, 0o600); err != nil {
		fmtLogf("写 guest.json 失败: %v", err)
		return
	}
	g.lastSave = time.Now()
	g.dirty = false
}

func flushGuest() {
	guestQuota.mu.Lock()
	guestQuota.saveLocked(true)
	guestQuota.mu.Unlock()
}

// isGuestRequest 没有账号、也不是已绑定设备。
func isGuestRequest(r *http.Request) bool {
	return resolveUserID(r) == ""
}

// countsAsNewPlay 只把「从头开始的那次请求」算一首。
func countsAsNewPlay(r *http.Request) bool {
	if r.Method == http.MethodHead {
		return false
	}
	rng := strings.TrimSpace(r.Header.Get("Range"))
	if rng == "" {
		return true
	}
	return strings.HasPrefix(rng, "bytes=0-")
}

func guestUsed(ip string) int {
	guestQuota.mu.Lock()
	defer guestQuota.mu.Unlock()
	guestQuota.enterLocked()
	return guestQuota.IPs[ip]
}

// guestRemaining 返回剩余可听数量；limit<=0 表示不限（返回 -1）。
func guestRemaining(r *http.Request) (limit, used, remaining int) {
	limit = guestDailyLimit()
	if limit <= 0 {
		return 0, 0, -1
	}
	used = guestUsed(clientIP(r))
	remaining = limit - used
	if remaining < 0 {
		remaining = 0
	}
	return limit, used, remaining
}

// checkGuestQuota 播放前检查，超了返回错误。
func checkGuestQuota(r *http.Request) (int, error) {
	limit := guestDailyLimit()
	if limit <= 0 {
		return -1, nil
	}
	ip := clientIP(r)
	used := guestUsed(ip)
	if used >= limit {
		return 0, fmt.Errorf("游客每日试听 %d 首已用完，登录后不限", limit)
	}
	return limit - used, nil
}

// noteGuestPlay 记一次游客试听。返回今日已用数量。
func noteGuestPlay(r *http.Request) int {
	ip := clientIP(r)
	guestQuota.mu.Lock()
	guestQuota.enterLocked()
	guestQuota.IPs[ip]++
	used := guestQuota.IPs[ip]
	guestQuota.dirty = true
	guestQuota.saveLocked(false)
	guestQuota.mu.Unlock()

	statsGuestPlay()
	return used
}

// HandleGuestQuota 前端用：当前这台机器还能试听几首。
func HandleGuestQuota(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	loggedIn := resolveUserID(r) != ""
	out := map[string]interface{}{
		"logged_in": loggedIn,
		"enabled":   guestDailyLimit() > 0,
		"limit":     guestDailyLimit(),
	}
	if !loggedIn && guestDailyLimit() > 0 {
		limit, used, remain := guestRemaining(r)
		out["limit"] = limit
		out["used"] = used
		out["remaining"] = remain
	}
	json.NewEncoder(w).Encode(out)
}

// guestSnapshot 给 /status 面板。
func guestSnapshot() map[string]interface{} {
	guestQuota.mu.Lock()
	defer guestQuota.mu.Unlock()
	guestQuota.enterLocked()
	var total int
	for _, n := range guestQuota.IPs {
		total += n
	}
	return map[string]interface{}{
		"limit":     guestDailyLimit(),
		"ips":       len(guestQuota.IPs),
		"total":     total,
		"day":       guestQuota.Day,
	}
}
