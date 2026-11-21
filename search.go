package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// SearchResult represents a search result with frontend-compatible fields
type SearchResult struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	URL      string `json:"url"`       // For frontend compatibility
	Link     string `json:"link"`      // Alternative field
	LyricURL string `json:"lyric_url"` // Lyrics URL
	CoverURL string `json:"cover_url"`
	Duration int    `json:"duration"`  // Duration in seconds
}

// HandleSearch handles music search requests
func HandleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	query := r.URL.Query().Get("query")
	fmt.Printf("[API] Search request: query=%s\n", query)

	if query == "" {
		json.NewEncoder(w).Encode([]SearchResult{})
		return
	}

	// 播放 URL 需要给浏览器用，补成绝对地址
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host

	// Search from multiple sources
	items := searchFromAllSources(query)

	// Convert to frontend-compatible format（歌名+歌手去重）
	results := make([]SearchResult, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		if strings.TrimSpace(item.Title) == "" {
			continue
		}
		key := item.Title + "\x00" + item.Artist
		if seen[key] {
			continue
		}
		seen[key] = true

		// 用 AudioFullURL 作为主播放地址，缺失时回退到 /stream_live 多源链路
		audioURL := item.AudioFullURL
		if audioURL == "" {
			audioURL = item.AudioURL
		}
		if audioURL == "" {
			audioURL = livePlayURL(item.Title, item.Artist)
		}
		if strings.HasPrefix(audioURL, "/") {
			audioURL = base + audioURL
		}

		coverURL := item.CoverURL
		if coverURL != "" && strings.HasPrefix(coverURL, "/") {
			coverURL = base + coverURL
		}

		results = append(results, SearchResult{
			Title:    item.Title,
			Artist:   item.Artist,
			URL:      audioURL,
			Link:     audioURL,
			LyricURL: item.LyricURL,
			CoverURL: coverURL,
			Duration: normalizeDuration(item.Duration),
		})

		fmt.Printf("[API] Result %d: title=%s, artist=%s, url=%s\n", len(results), item.Title, item.Artist, audioURL)
	}

	fmt.Printf("[API] Search completed: %d results found\n", len(results))
	json.NewEncoder(w).Encode(results)
}

// livePlayURL 生成播放地址。候选只提供「歌名 + 歌手」，真正的取链交给
// /stream_live —— 它走 requestAndCacheMusic 的多源链路（酷我/网易云/QQ/
// 酷狗/xfabe/mp3api…），和 ESP32 设备播放用的是同一条已验证可用的链路。
func livePlayURL(title, artist string) string {
	return "/stream_live?song=" + url.QueryEscape(title) + "&singer=" + url.QueryEscape(artist)
}

// normalizeDuration 把毫秒时长归一成秒（各音源单位不一致）。
func normalizeDuration(d int) int {
	if d > 10000 { // 明显是毫秒
		return d / 1000
	}
	return d
}

// searchFromAllSources searches music from all available sources
func searchFromAllSources(query string) []MusicItem {
	var results []MusicItem

	// Source 1: Search from sources.json (local config)
	sources := readSources()
	for _, source := range sources {
		if containsIgnoreCase(source.Title, query) || containsIgnoreCase(source.Artist, query) {
			item := MusicItem{
				Title:        source.Title,
				Artist:       source.Artist,
				AudioURL:     source.AudioURL,
				AudioFullURL: source.AudioFullURL,
				M3U8URL:      source.M3U8URL,
				LyricURL:     source.LyricURL,
				CoverURL:     source.CoverURL,
				Duration:     source.Duration,
				FromCache:    false,
			}
			results = append(results, item)

			// Limit results to prevent too many items
			if len(results) >= 20 {
				break
			}
		}
	}

	// Source 2: Search from local files
	localItems := searchLocalMusic(query)
	results = append(results, localItems...)

	// 本地没有足够结果时，走在线音源
	if len(results) < 8 {
		apiResults := searchFromAPI(query)
		results = append(results, apiResults...)
	}

	// Limit total results
	if len(results) > 20 {
		results = results[:20]
	}

	return results
}

// searchLocalMusic searches music from local files
func searchLocalMusic(query string) []MusicItem {
	// This function would search from local music files
	// For now, return empty array
	// TODO: Implement local file search
	return []MusicItem{}
}

// searchFromAPI 从在线音源搜索候选列表。
//
// 历史问题：这里原本只调「雨风/妖狐」免费聚合 API（YuafengAPIResponseHandler），
// 2026-11 实测三家域名全部失效：api.yuafeng.cn 提示「站点已迁移」、
// api-v2.yuafeng.cn 404、api.yaohud.cn 404。结果 Web 搜索恒为空，
// 但播放链路 /stream_live 用的是另一批音源所以「能播却搜不到」。
//
// 修复：复用播放链路里已在用的音源搜索函数（kwSearch / neSearch /
// qqSearch / kgSearch），它们与 requestAndCacheMusic 同源、稳定可用。
// 返回的候选只带歌名/歌手/时长，播放 URL 指向 /stream_live，便于复用
// 那条已经验证过的取链 + 缓存 + 转码链路。
func searchFromAPI(query string) []MusicItem {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	var results []MusicItem
	add := func(title, artist string, duration int) {
		title = strings.TrimSpace(title)
		if title == "" {
			return
		}
		results = append(results, MusicItem{
			Title:        title,
			Artist:       strings.TrimSpace(artist),
			Duration:     duration,
			AudioURL:     livePlayURL(title, artist),
			AudioFullURL: livePlayURL(title, artist),
		})
	}

	// 主源：酷我（播放链路首选，搜索接口实测可用、延迟低）
	if srcEnabled("kuwo") {
		fmt.Printf("[Search] source=kuwo query=%s\n", query)
		for _, s := range kwSearch(query, "") {
			add(s.Name, s.Artist, s.Duration)
			if len(results) >= 20 {
				break
			}
		}
	}

	// 兜底 1：网易云直连（无版权过滤，能补酷我搜不到的歌）
	if len(results) < 8 && srcEnabled("netease") {
		fmt.Printf("[Search] source=netease query=%s\n", query)
		for _, s := range neSearch(query, "") {
			add(s.Name, s.Artist, s.Duration)
			if len(results) >= 20 {
				break
			}
		}
	}

	// 兜底 2：QQ 音乐
	if len(results) < 8 && srcEnabled("qq") {
		fmt.Printf("[Search] source=qq query=%s\n", query)
		for _, s := range qqSearch(query, "") {
			add(s.Name, s.Artist, s.Duration)
			if len(results) >= 20 {
				break
			}
		}
	}

	// 兜底 3：酷狗
	if len(results) < 8 && srcEnabled("kugou") {
		fmt.Printf("[Search] source=kugou query=%s\n", query)
		for _, s := range kgSearch(query, "") {
			add(s.Name, s.Artist, s.Duration)
			if len(results) >= 20 {
				break
			}
		}
	}

	fmt.Printf("[Search] online sources returned %d candidates\n", len(results))
	return results
}

// containsIgnoreCase checks if a string contains substring (case insensitive)
func containsIgnoreCase(s, substr string) bool {
	s = toLower(s)
	substr = toLower(substr)
	return contains(s, substr)
}

func toLower(s string) string {
	var result []rune
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 32
		}
		result = append(result, r)
	}
	return string(result)
}

func contains(s, substr string) bool {
	return len(substr) == 0 || indexString(s, substr) >= 0
}

func indexString(s, substr string) int {
	n := len(substr)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == substr {
			return i
		}
	}
	return -1
}
