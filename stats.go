package main

// 播放统计
//
// files/history.json 只存「每首歌听了几次」，是聚合快照，看不出「这几天听得
// 多不多」「哪个时段最常听」。这里单独记一份按天的计数器，给 /status 画图：
//
//   - days   每天 {plays, transcode, guest, songs}，保留 90 天
//   - hours  24 个小时桶（累计），看出「深夜听歌」还是「通勤听」
//   - artists / songs  累计次数 top 榜，只留前 300 名，不无限膨胀
//
// 写盘 15 秒节流：听歌高峰时 recordPlay 一瞬间会被打几十次，不必每次都落盘。
// 数据失败不影响播放（所有写操作都是「尽力而为」）。

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	statsFilePath = "./files/stats.json"
	statsKeepDays = 90
	statsTopMax   = 300
	statsSaveGap  = 15 * time.Second
)

type dayCount struct {
	Plays     int `json:"plays"`
	Transcode int `json:"transcode"`
	Guest     int `json:"guest"`
	Songs     int `json:"songs"` // 当天第一次听的不同歌曲数
}

type statsStore struct {
	mu sync.Mutex

	Days     map[string]*dayCount `json:"days"`
	Artists  map[string]int       `json:"artists"`
	Songs    map[string]int       `json:"songs"`
	Hours    []int                `json:"hours"`
	Total    int                  `json:"total_plays"`
	TotalEnc int                  `json:"total_transcodes"`

	// 当天已经算过「不同歌曲」的键，只放内存（重启后当天去重会重算，图表无感）
	daySeen map[string]map[string]struct{}

	lastSave time.Time
	dirty    bool
}

var playStats = &statsStore{
	Days:    map[string]*dayCount{},
	Artists: map[string]int{},
	Songs:   map[string]int{},
	Hours:   make([]int, 24),
	daySeen: map[string]map[string]struct{}{},
}

func initStatsStore() {
	data, err := os.ReadFile(statsFilePath)
	if err == nil {
		var loaded statsStore
		if json.Unmarshal(data, &loaded) == nil {
			if loaded.Days != nil {
				playStats.Days = loaded.Days
			}
			if loaded.Artists != nil {
				playStats.Artists = loaded.Artists
			}
			if loaded.Songs != nil {
				playStats.Songs = loaded.Songs
			}
			if len(loaded.Hours) == 24 {
				playStats.Hours = loaded.Hours
			}
			playStats.Total = loaded.Total
			playStats.TotalEnc = loaded.TotalEnc
		}
	}
	playStats.mu.Lock()
	pruneStatsLocked()
	playStats.mu.Unlock()
}

func (s *statsStore) dayLocked(day string) *dayCount {
	d := s.Days[day]
	if d == nil {
		d = &dayCount{}
		s.Days[day] = d
	}
	return d
}

func (s *statsStore) saveLocked(force bool) {
	if !s.dirty {
		return
	}
	if !force && time.Since(s.lastSave) < statsSaveGap {
		return
	}
	data, err := json.Marshal(struct {
		Days     map[string]*dayCount `json:"days"`
		Artists  map[string]int       `json:"artists"`
		Songs    map[string]int       `json:"songs"`
		Hours    []int                `json:"hours"`
		Total    int                  `json:"total_plays"`
		TotalEnc int                  `json:"total_transcodes"`
	}{s.Days, s.Artists, s.Songs, s.Hours, s.Total, s.TotalEnc})
	if err != nil {
		return
	}
	if err := writeFileAtomic(statsFilePath, data, 0o600); err != nil {
		fmtLogf("写 stats.json 失败: %v", err)
		return
	}
	s.lastSave = time.Now()
	s.dirty = false
}

// pruneStatsLocked 丢掉过期天数、砍掉长尾榜单。调用方持锁。
func pruneStatsLocked() {
	cutoff := time.Now().AddDate(0, 0, -statsKeepDays).Format("2006-01-02")
	for day := range playStats.Days {
		if day < cutoff {
			delete(playStats.Days, day)
		}
	}
	trimTopMap(playStats.Artists, statsTopMax)
	trimTopMap(playStats.Songs, statsTopMax)
}

func trimTopMap(m map[string]int, keep int) {
	if len(m) <= keep {
		return
	}
	type kv struct {
		k string
		v int
	}
	list := make([]kv, 0, len(m))
	for k, v := range m {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	for i := keep; i < len(list); i++ {
		delete(m, list[i].k)
	}
}

// statsPlay 记一次「真的听进去了」的播放（和历史记录同一个触发点）。
func statsPlay(artist, title string) {
	s := playStats
	s.mu.Lock()
	defer s.mu.Unlock()

	day := todayKey()
	d := s.dayLocked(day)
	d.Plays++
	s.Total++

	hour := time.Now().Hour()
	if hour >= 0 && hour < len(s.Hours) {
		s.Hours[hour]++
	}

	artist = strings.TrimSpace(artist)
	if artist != "" {
		s.Artists[artist]++
	}
	if title != "" {
		label := title
		if artist != "" {
			label = artist + " - " + title
		}
		s.Songs[label]++
	}

	// 当天去重曲目数
	seen := s.daySeen[day]
	if seen == nil {
		seen = map[string]struct{}{}
		s.daySeen[day] = seen
	}
	key := historyKey(artist, title)
	if _, ok := seen[key]; !ok {
		seen[key] = struct{}{}
		d.Songs = len(seen)
	}

	pruneStatsLocked()
	s.dirty = true
	s.saveLocked(false)
}

// statsTranscode 记一次真正启动了 ffmpeg 的转码（命中缓存不算）。
func statsTranscode() {
	s := playStats
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.dayLocked(todayKey())
	d.Transcode++
	s.TotalEnc++
	s.dirty = true
	s.saveLocked(false)
}

// statsGuestPlay 记一次游客试听。
func statsGuestPlay() {
	s := playStats
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dayLocked(todayKey()).Guest++
	s.dirty = true
	s.saveLocked(false)
}

// flushStats 退出前落盘。
func flushStats() {
	playStats.mu.Lock()
	playStats.saveLocked(true)
	playStats.mu.Unlock()
}

// ---------- 对外接口 ----------

type statsPoint struct {
	Date      string `json:"date"`  // 11-22
	Full      string `json:"full"`  // 2026-11-22
	Weekday   string `json:"weekday"`
	Plays     int    `json:"plays"`
	Transcode int    `json:"transcode"`
	Guest     int    `json:"guest"`
	Songs     int    `json:"songs"`
}

type statsNameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// HandlePlayStats 给 /status 面板的图表接口。
// GET /api/stats?days=14   （days 1..90，默认 14）
func HandlePlayStats(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= statsKeepDays {
			days = n
		}
	}

	s := playStats
	s.mu.Lock()

	series := make([]statsPoint, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		t := now.AddDate(0, 0, -i)
		full := t.Format("2006-01-02")
		p := statsPoint{
			Date:    t.Format("01-02"),
			Full:    full,
			Weekday: []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(t.Weekday())],
		}
		if d := s.Days[full]; d != nil {
			p.Plays = d.Plays
			p.Transcode = d.Transcode
			p.Guest = d.Guest
			p.Songs = d.Songs
		}
		series = append(series, p)
	}

	topArtists := make([]statsNameCount, 0, 8)
	for k, v := range s.Artists {
		topArtists = append(topArtists, statsNameCount{k, v})
	}
	sort.Slice(topArtists, func(i, j int) bool {
		if topArtists[i].Count != topArtists[j].Count {
			return topArtists[i].Count > topArtists[j].Count
		}
		return topArtists[i].Name < topArtists[j].Name
	})
	if len(topArtists) > 8 {
		topArtists = topArtists[:8]
	}

	topSongs := make([]statsNameCount, 0, 8)
	for k, v := range s.Songs {
		topSongs = append(topSongs, statsNameCount{k, v})
	}
	sort.Slice(topSongs, func(i, j int) bool {
		if topSongs[i].Count != topSongs[j].Count {
			return topSongs[i].Count > topSongs[j].Count
		}
		return topSongs[i].Name < topSongs[j].Name
	})
	if len(topSongs) > 8 {
		topSongs = topSongs[:8]
	}

	hours := make([]int, 24)
	copy(hours, s.Hours)
	today := dayCount{}
	if d := s.Days[todayKey()]; d != nil {
		today = *d
	}
	total := s.Total
	totalEnc := s.TotalEnc
	s.mu.Unlock()

	// 近 7 天与昨天：卡片上直接显示「趋势」
	var last7, prev7 int
	for i, p := range series {
		if i >= len(series)-7 {
			last7 += p.Plays
		} else if i >= len(series)-14 {
			prev7 += p.Plays
		}
	}
	var yesterday int
	if len(series) >= 2 {
		yesterday = series[len(series)-2].Plays
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"days":      series,
		"top_artists": topArtists,
		"top_songs":   topSongs,
		"hours":       hours,
		"today": map[string]interface{}{
			"plays":     today.Plays,
			"transcode": today.Transcode,
			"guest":     today.Guest,
			"songs":     today.Songs,
		},
		"total_plays":      total,
		"total_transcodes": totalEnc,
		"last7":            last7,
		"prev7":            prev7,
		"yesterday":        yesterday,
	})
}
