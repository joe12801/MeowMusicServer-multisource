package main

// 正在播放（/status 顶部「现在在听什么」）
//
// 播放历史和统计都是「听完才算一次」，/status 上只能看到昨天的柱子和累计榜，
// 看不到「此刻放的哪首」。这里让播放器每 10 秒心跳一次（切歌/暂停也立刻报），
// 纯内存保存：
//
//   - key 用「用户 + 浏览器标签」，同一账号开两个页面互不覆盖；
//   - 30 分钟没有心跳就当作走了（关页面、断网都不会留僵尸行）；
//   - 进程重启清零，和音源健康一样只反映当前进程；
//   - 只有一首歌真的在响（playing=true 且 90 秒内有心跳）才标「播放中」，
//     暂停时变灰但仍然显示，方便看「放到哪了」。
//
// 接口：
//   POST /api/now   {title,artist,cover_url,source,duration,position,playing,client}  需登录
//   GET  /api/now   {items:[],server_time}                                            登录或应急口令

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	nowAliveWindow = 30 * time.Minute // 心跳超过这个时长就当人走了
	nowFreshWindow = 45 * time.Second // 心跳在这个窗口内才算「正在播放」（8 秒一次心跳，丢 5 次才判停）
	nowMaxEntries  = 64
	nowListMax     = 8
)

type nowEntry struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Cover    string `json:"cover_url,omitempty"`
	Source   string `json:"source,omitempty"`
	Duration int    `json:"duration"`
	Position int    `json:"position"`
	Playing  bool   `json:"playing"`
	Fresh    bool   `json:"fresh"` // 心跳够新，才算真的在放
	Client   string `json:"client,omitempty"`
	UserID   string `json:"-"`
	User     string `json:"user,omitempty"`
	Updated  int64  `json:"updated_at"` // Unix 秒
}

type nowStore struct {
	mu    sync.Mutex
	items map[string]*nowEntry // userID|client -> entry
}

var nowPlayingBox = &nowStore{items: map[string]*nowEntry{}}

// usernameByID 反查用户名：Usernames 是「名字 -> ID」，量小，线性找一下就好。
func usernameByID(id string) string {
	if userStore == nil || id == "" {
		return ""
	}
	userStore.mu.RLock()
	defer userStore.mu.RUnlock()
	for name, uid := range userStore.Usernames {
		if uid == id {
			return name
		}
	}
	return ""
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// sanitizeClient 客户端标识只留安全字符，防止被当 HTML/日志注入。
func sanitizeClient(s string) string {
	s = strings.TrimSpace(s)
	out := make([]rune, 0, 24)
	for _, r := range s {
		if len(out) >= 24 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "web"
	}
	return string(out)
}

// HandleNowReport 播放器心跳。
func HandleNowReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := resolveUserID(r)
	if userID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var body struct {
		Title    string `json:"title"`
		Artist   string `json:"artist"`
		CoverURL string `json:"cover_url"`
		Source   string `json:"source"`
		Duration int    `json:"duration"`
		Position int    `json:"position"`
		Playing  bool   `json:"playing"`
		Client   string `json:"client"`
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
	cover := body.CoverURL
	if len(cover) > 500 || !(strings.HasPrefix(cover, "http://") || strings.HasPrefix(cover, "https://")) {
		cover = ""
	}

	e := &nowEntry{
		Title:    title,
		Artist:   artist,
		Cover:    cover,
		Source:   normalizeSourceLabel(body.Source),
		Duration: clampInt(body.Duration, 0, 24*3600),
		Position: clampInt(body.Position, 0, 24*3600),
		Playing:  body.Playing,
		Client:   sanitizeClient(body.Client),
		UserID:   userID,
		Updated:  time.Now().Unix(),
	}

	box := nowPlayingBox
	box.mu.Lock()
	key := userID + "|" + e.Client
	if len(box.items) >= nowMaxEntries {
		if _, exists := box.items[key]; !exists {
			pruneNowLocked(box)
			// 还是满（全部都很新）就挤掉最旧的
			if len(box.items) >= nowMaxEntries {
				var oldestKey string
				var oldest int64
				for k, v := range box.items {
					if oldestKey == "" || v.Updated < oldest {
						oldestKey, oldest = k, v.Updated
					}
				}
				delete(box.items, oldestKey)
			}
		}
	}
	box.items[key] = e
	box.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// pruneNowLocked 丢掉 30 分钟没心跳的。调用方持锁。
func pruneNowLocked(box *nowStore) {
	cut := time.Now().Add(-nowAliveWindow).Unix()
	for k, v := range box.items {
		if v.Updated < cut {
			delete(box.items, k)
		}
	}
}

// HandleNowList 给 /status 读的列表。
func HandleNowList(w http.ResponseWriter, r *http.Request) {
	box := nowPlayingBox
	now := time.Now()
	freshCut := now.Add(-nowFreshWindow).Unix()

	box.mu.Lock()
	pruneNowLocked(box)
	list := make([]nowEntry, 0, len(box.items))
	for _, v := range box.items {
		cp := *v
		cp.Fresh = cp.Playing && cp.Updated >= freshCut
		cp.User = usernameByID(cp.UserID)
		list = append(list, cp)
	}
	box.mu.Unlock()

	// 在放的排前面，然后按最近心跳
	sort.Slice(list, func(i, j int) bool {
		if list[i].Fresh != list[j].Fresh {
			return list[i].Fresh
		}
		return list[i].Updated > list[j].Updated
	})
	if len(list) > nowListMax {
		list = list[:nowListMax]
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items":       list,
		"server_time": now.Unix(),
	})
}
