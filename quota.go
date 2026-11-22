package main

// 每日转码额度
//
// 问题：/stream_live 是公开接口，任何人知道歌名就能让服务器跑 ffmpeg。
// 一旦服务器地址被爬或者被转发到群里，几百个请求同时进来 —— 2 核 NAS
// 直接被转码打满，正常听歌反而卡死。
//
// 做法：按「身份」统计每天成功转码的歌曲数，超过上限直接 429，不启动 ffmpeg。
//
//   - 已登录（Bearer / Cookie）→ 记在用户名下；
//   - 带设备 Token 的请求（ESP32）→ 记在设备归属用户名下，不额外占额度；
//   - 匿名请求 → 记在来源 IP 名下。
//
// 默认值：每 IP 500 首/天（约 25 小时音乐，正常家庭用不到），
// 每用户 0 = 不限制（自有账号不给上限，避免误伤自己）。
// 想关掉某个维度就设成 0。
//
// 计数落盘到 files/usage.json，进程重启不清零；跨天自动重置。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type quotaUsage struct {
	Songs int   `json:"songs"`
	Bytes int64 `json:"bytes"`
}

type quotaStore struct {
	mu       sync.Mutex
	Day      string                 `json:"day"`
	Items    map[string]*quotaUsage `json:"items"`
	lastSave time.Time
	dirty    bool
}

var transcodeQuota = &quotaStore{Items: map[string]*quotaUsage{}}

const quotaFilePath = "./files/usage.json"

func initQuotaStore() {
	data, err := os.ReadFile(quotaFilePath)
	if err == nil {
		var loaded quotaStore
		if json.Unmarshal(data, &loaded) == nil && loaded.Items != nil {
			transcodeQuota.Day = loaded.Day
			transcodeQuota.Items = loaded.Items
		}
	}
	if transcodeQuota.Items == nil {
		transcodeQuota.Items = map[string]*quotaUsage{}
	}
	// 跨天启动：计数作废，文件留到下次写入时覆盖
	if transcodeQuota.Day != todayKey() {
		transcodeQuota.Items = map[string]*quotaUsage{}
		transcodeQuota.Day = todayKey()
	}
	fmt.Printf("[Quota] 每日额度：IP=%s 用户=%s（0=不限）\n",
		limitDesc(quotaLimitPerIP()), limitDesc(quotaLimitPerUser()))
}

func todayKey() string {
	return time.Now().Format("2006-01-02")
}

func limitDesc(n int) string {
	if n <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d 首/天", n)
}

func quotaLimitPerIP() int {
	n := envInt("TRANSCODE_DAILY_PER_IP", 500)
	if n < 0 {
		return 0
	}
	return n
}

func quotaLimitPerUser() int {
	n := envInt("TRANSCODE_DAILY_PER_USER", 0)
	if n < 0 {
		return 0
	}
	return n
}

// enterLocked 跨天时清空计数（进程长期运行也要换日）
func (q *quotaStore) enterLocked() {
	today := todayKey()
	if q.Day != today {
		q.Day = today
		q.Items = map[string]*quotaUsage{}
		q.dirty = true
	}
}

func (q *quotaStore) songs(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enterLocked()
	if u, ok := q.Items[key]; ok {
		return u.Songs
	}
	return 0
}

func (q *quotaStore) add(key string, bytes int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enterLocked()

	u, ok := q.Items[key]
	if !ok {
		u = &quotaUsage{}
		q.Items[key] = u
	}
	u.Songs++
	u.Bytes += bytes
	q.dirty = true

	// 限流地写盘：每 10 秒最多一次，避免每首歌都 fsync
	if time.Since(q.lastSave) > 10*time.Second {
		q.saveLocked()
	}
	return u.Songs
}

func (q *quotaStore) saveLocked() {
	data, err := json.MarshalIndent(struct {
		Day   string                 `json:"day"`
		Items map[string]*quotaUsage `json:"items"`
	}{q.Day, q.Items}, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(quotaFilePath, data, 0o600); err != nil {
		fmtLogf("写 usage.json 失败: %v", err)
		return
	}
	q.lastSave = time.Now()
	q.dirty = false
}

// flushQuota 退出前调用，把最后几秒的计数落盘
func flushQuota() {
	transcodeQuota.mu.Lock()
	defer transcodeQuota.mu.Unlock()
	if transcodeQuota.dirty {
		transcodeQuota.saveLocked()
	}
}

// quotaKeyFor 认定这次请求记在谁头上。
// 返回的 key 形如 "user:joe1280" / "ip:1.2.3.4"，label 用于日志与面板。
func quotaKeyFor(r *http.Request) (key, label string) {
	if user := pageSessionUser(r); user != "" {
		return "user:" + strings.ToLower(user), user
	}
	if dev, err := deviceFromRequest(r); err == nil && dev.Username != "" {
		return "user:" + strings.ToLower(dev.Username), "设备 " + dev.MAC
	}
	ip := clientIP(r)
	return "ip:" + ip, ip
}

// checkTranscodeQuota 转码前检查：超了就不启动 ffmpeg。
func checkTranscodeQuota(r *http.Request) error {
	key, label := quotaKeyFor(r)

	limit := quotaLimitPerUser()
	if strings.HasPrefix(key, "ip:") {
		limit = quotaLimitPerIP()
	}
	if limit <= 0 {
		return nil
	}

	used := transcodeQuota.songs(key)
	if used >= limit {
		fmtLogf("转码额度用尽：%s 已用 %d/%d", label, used, limit)
		return fmt.Errorf("今日转码额度已用完（%d/%d 首），明天自动恢复", used, limit)
	}
	return nil
}

// noteTranscode 转码成功后记账。
func noteTranscode(r *http.Request, bytes int64) {
	key, label := quotaKeyFor(r)
	n := transcodeQuota.add(key, bytes)
	statsTranscode()
	limit := quotaLimitPerUser()
	if strings.HasPrefix(key, "ip:") {
		limit = quotaLimitPerIP()
	}
	if limit > 0 && n == limit {
		fmtLogf("%s 今日转码额度已用满（%d/%d）", label, n, limit)
	}
}

// quotaSnapshot 给 /api/sources 用：不暴露全部 IP，只给汇总和前几名。
func quotaSnapshot() map[string]interface{} {
	transcodeQuota.mu.Lock()
	defer transcodeQuota.mu.Unlock()
	transcodeQuota.enterLocked()

	type row struct {
		Key   string `json:"key"`
		Songs int    `json:"songs"`
		Bytes int64  `json:"bytes"`
	}
	rows := make([]row, 0, len(transcodeQuota.Items))
	var totalSongs int
	for k, u := range transcodeQuota.Items {
		rows = append(rows, row{k, u.Songs, u.Bytes})
		totalSongs += u.Songs
	}
	// 取用量前 5 名，条目很少，标准库排序即可
	sort.Slice(rows, func(i, j int) bool { return rows[i].Songs > rows[j].Songs })
	top := rows
	if len(top) > 5 {
		top = top[:5]
	}

	return map[string]interface{}{
		"day":         transcodeQuota.Day,
		"keys":        len(transcodeQuota.Items),
		"total_songs": totalSongs,
		"per_ip":      quotaLimitPerIP(),
		"per_user":    quotaLimitPerUser(),
		"top":         top,
	}
}
