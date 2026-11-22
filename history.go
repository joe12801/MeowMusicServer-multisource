package main

// 播放历史与「最近常听」
//
// 原来一首歌播完什么都不留：首页只有搜索框和「我喜欢」，想再听昨天那首
// 得凭记忆重搜一遍。这里按用户记录播放次数与最后播放时间：
//
//   - 前端在真正开始播放 20 秒后上报一次（<audio> 的 timeupdate 驱动），
//     拖进度条、暂停继续都不会重复计数；
//   - 同一首歌按「歌手 + 歌名」聚合，只更新 count / last_at，不存流水，
//     所以文件不会随着听歌次数膨胀；
//   - 每个用户最多保留 800 首，超了按最后播放时间裁掉最旧的；
//   - 落盘 files/history.json，写盘 10 秒节流（连续切歌不会一直写文件）。
//
// 接口：
//   POST /api/history/report   {title, artist, duration, cover_url}   需登录
//   GET  /api/history?limit=   {recent:[], top:[], total, since}      需登录
//   POST /api/history/clear    清空当前用户                      需登录

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	historyFilePath = "./files/history.json"
	historyMaxSongs = 800
	historySaveGap  = 10 * time.Second
)

// HistorySong 一首歌在某个用户名下的聚合记录。
type HistorySong struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration,omitempty"`
	Cover    string `json:"cover_url,omitempty"`
	Source   string `json:"source,omitempty"` // 播放时用的音源（kuwo/netease/qq/kugou/ext）
	Count    int    `json:"count"`
	LastAt   int64  `json:"last_at"`
	FirstAt  int64  `json:"first_at"`

	// 返回给前端时现算，不落盘
	URL      string `json:"url,omitempty"`
	LyricURL string `json:"lyric_url,omitempty"`
}

type historyStore struct {
	mu       sync.Mutex
	Users    map[string]map[string]*HistorySong `json:"users"` // userID -> dedupeKey -> song
	lastSave time.Time
	dirty    bool
}

var playHistory = &historyStore{Users: map[string]map[string]*HistorySong{}}

// historyKey 聚合同一首歌：大小写、首尾空格、多空格都不该分成两条。
func historyKey(artist, title string) string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		return strings.Join(strings.Fields(s), " ")
	}
	return norm(artist) + "\x1f" + norm(title)
}

func initHistoryStore() {
	data, err := os.ReadFile(historyFilePath)
	if err != nil {
		return
	}
	var loaded historyStore
	if json.Unmarshal(data, &loaded) != nil || loaded.Users == nil {
		return
	}
	playHistory.mu.Lock()
	playHistory.Users = loaded.Users
	playHistory.mu.Unlock()
}

// saveHistoryLocked 调用方需持有锁。
func saveHistoryLocked(force bool) {
	if !playHistory.dirty {
		return
	}
	if !force && time.Since(playHistory.lastSave) < historySaveGap {
		return
	}
	data, err := json.MarshalIndent(playHistory.Users, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(historyFilePath, data, 0o600); err != nil {
		return
	}
	playHistory.lastSave = time.Now()
	playHistory.dirty = false
}

// flushHistory 退出前把未落盘的记录写掉。
func flushHistory() {
	playHistory.mu.Lock()
	saveHistoryLocked(true)
	playHistory.mu.Unlock()
}

// recordPlay 记一次播放。
func recordPlay(userID, title, artist string, duration int, cover, source string) {
	bucket := playHistory.Users[userID]
	if bucket == nil {
		bucket = map[string]*HistorySong{}
		playHistory.Users[userID] = bucket
	}
	key := historyKey(artist, title)
	now := time.Now().Unix()

	s := bucket[key]
	if s == nil {
		s = &HistorySong{Title: title, Artist: artist, FirstAt: now}
		bucket[key] = s
	}
	s.Count++
	s.LastAt = now
	if duration > 0 {
		s.Duration = duration
	}
	if cover != "" {
		s.Cover = cover
	}
	if source != "" {
		s.Source = source
	}
	// 歌名要以最后一次上报的写法为准（用户搜「晴天」和「晴天 (Live)」不会混，
	// 但同一首歌的不同写法应该稳定显示最近一次的）
	if title != "" {
		s.Title = title
	}
	if artist != "" {
		s.Artist = artist
	}

	// 超出上限就裁掉最久没听的
	if len(bucket) > historyMaxSongs {
		list := make([]*HistorySong, 0, len(bucket))
		for _, v := range bucket {
			list = append(list, v)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].LastAt > list[j].LastAt })
		keep := map[string]struct{}{}
		for i := 0; i < historyMaxSongs && i < len(list); i++ {
			keep[historyKey(list[i].Artist, list[i].Title)] = struct{}{}
		}
		for k := range bucket {
			if _, ok := keep[k]; !ok {
				delete(bucket, k)
			}
		}
	}

	playHistory.dirty = true
	saveHistoryLocked(false)

	// 顺带喂给 /status 的统计图表（独立锁，不会和这里的锁打架）
	statsPlay(artist, title)
}

// historyView 把内部结构转成前端可直接播的形态。
func historyView(s *HistorySong) HistorySong {
	out := *s
	q := url.Values{}
	q.Set("song", s.Title)
	q.Set("singer", s.Artist)
	out.URL = "/stream_live?" + q.Encode()
	lq := url.Values{}
	lq.Set("song", s.Title)
	lq.Set("singer", s.Artist)
	lq.Set("trans", "1")
	out.LyricURL = "/api/lyric?" + lq.Encode()
	return out
}

// HandleHistoryReport 记录一次播放。
func HandleHistoryReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := resolveUserID(r)
	if userID == "" {
		// 没登录就没什么可记的：前端本地也存一份「本次设备最近播放」，
		// 但跨设备同步只对有账号的人有意义。
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var body struct {
		Title    string `json:"title"`
		Artist   string `json:"artist"`
		Duration int    `json:"duration"`
		CoverURL string `json:"cover_url"`
		Source   string `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" || len(title) > 200 {
		http.Error(w, "Invalid title", http.StatusBadRequest)
		return
	}
	artist := strings.TrimSpace(body.Artist)
	if len(artist) > 200 {
		artist = artist[:200]
	}
	if body.Duration < 0 || body.Duration > 24*3600 {
		body.Duration = 0
	}
	cover := body.CoverURL
	if len(cover) > 500 || !(strings.HasPrefix(cover, "http://") || strings.HasPrefix(cover, "https://")) {
		cover = ""
	}

	playHistory.mu.Lock()
	recordPlay(userID, title, artist, body.Duration, cover, normalizeSourceLabel(body.Source))
	playHistory.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// normalizeSourceLabel 只放行已知音源名，避免前端随便塞字符串进数据文件。
func normalizeSourceLabel(src string) string {
	src = strings.ToLower(strings.TrimSpace(src))
	switch src {
	case "kuwo", "netease", "qq", "kugou", "ext", "qishui", "ximalaya", "fivesing":
		return src
	}
	return ""
}

// HandleHistory 返回最近播放 + 最常听。
func HandleHistory(w http.ResponseWriter, r *http.Request) {
	userID := resolveUserID(r)
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	limit := 30
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	playHistory.mu.Lock()
	all := make([]HistorySong, 0, len(playHistory.Users[userID]))
	for _, s := range playHistory.Users[userID] {
		all = append(all, historyView(s))
	}
	playHistory.mu.Unlock()

	// 最近播放：按最后播放时间倒序
	recent := make([]HistorySong, len(all))
	copy(recent, all)
	sort.Slice(recent, func(i, j int) bool { return recent[i].LastAt > recent[j].LastAt })
	if len(recent) > limit {
		recent = recent[:limit]
	}

	// 最常听：次数倒序，次数相同按最近听的排前面
	top := make([]HistorySong, len(all))
	copy(top, all)
	sort.Slice(top, func(i, j int) bool {
		if top[i].Count != top[j].Count {
			return top[i].Count > top[j].Count
		}
		return top[i].LastAt > top[j].LastAt
	})
	if len(top) > limit {
		top = top[:limit]
	}

	var totalPlays int
	for _, s := range all {
		totalPlays += s.Count
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"recent":      recent,
		"top":         top,
		"total_songs": len(all),
		"total_plays": totalPlays,
	})
}

// HandleHistoryClear 清空当前用户的播放历史。
func HandleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := resolveUserID(r)
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	playHistory.mu.Lock()
	delete(playHistory.Users, userID)
	playHistory.dirty = true
	saveHistoryLocked(true)
	playHistory.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// historyStats 给 /api/sources 的运维面板用。
func historyStats() map[string]interface{} {
	playHistory.mu.Lock()
	defer playHistory.mu.Unlock()
	var songs, plays int
	for _, bucket := range playHistory.Users {
		songs += len(bucket)
		for _, s := range bucket {
			plays += s.Count
		}
	}
	return map[string]interface{}{
		"users": len(playHistory.Users),
		"songs": songs,
		"plays": plays,
	}
}
