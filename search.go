package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SearchResult represents a search result with frontend-compatible fields
type SearchResult struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	URL      string `json:"url"`       // For frontend compatibility
	Link     string `json:"link"`      // Alternative field
	LyricURL string `json:"lyric_url"` // Lyrics URL
	CoverURL string `json:"cover_url"`
	Duration int    `json:"duration"` // Duration in seconds
	// Source = "ext" 表示来自外部源（YouTube/mp3api），前端会标出来
	Source string `json:"source,omitempty"`
}

// HandleSearch handles music search requests
func HandleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	query := r.URL.Query().Get("query")
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	limit := parseSearchLimit(r.URL.Query().Get("limit"))
	fmt.Printf("[API] Search request: query=%s artist=%s limit=%d\n", query, artist, limit)

	if query == "" && artist == "" {
		json.NewEncoder(w).Encode([]SearchResult{})
		return
	}

	// 播放 URL 需要给浏览器用，补成绝对地址
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host

	// Search from multiple sources（歌名+歌手，歌手可为空=只按歌手搜）
	items := searchFromAllSources(query, artist, limit, strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source"))))

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

		// 歌词：各音源自带的 lyric_url 基本都是空的，
		// 统一指向本机 /api/lyric（按需从网易云拉 LRC 并缓存）。
		lyricURL := item.LyricURL
		if lyricURL == "" && strings.TrimSpace(item.Title) != "" {
			lyricURL = base + "/api/lyric?song=" + url.QueryEscape(item.Title) +
				"&singer=" + url.QueryEscape(item.Artist) + "&format=lrc"
		}

		results = append(results, SearchResult{
			Title:    item.Title,
			Artist:   item.Artist,
			URL:      audioURL,
			Link:     audioURL,
			LyricURL: lyricURL,
			CoverURL: coverURL,
			Duration: normalizeDuration(item.Duration),
			Source:   item.Source,
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

// 搜索结果条数：默认 20（老行为），Web 端可手动指定，最大 500。
const (
	searchDefaultLimit = 20
	searchMaxLimit     = 500
)

func parseSearchLimit(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return searchDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return searchDefaultLimit
	}
	if n > searchMaxLimit {
		return searchMaxLimit
	}
	return n
}

// searchFromAllSources searches music from all available sources
// query=歌名(可为空), artist=歌手(可为空)。两者至少一个非空。
// limit 是期望返回的候选条数（前端可手动指定，默认 20）。
func searchFromAllSources(query, artist string, limit int, only string) []MusicItem {
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	// 指定单一音源：汽水 / YouTube 这类补充源的曲库和主源基本不重叠，
	// 混在「全部」里很难排到前面，单独选出来搜才有意义。
	if only != "" {
		return searchOnlySource(only, query, artist, limit)
	}

	// 只按歌手搜时多取一些再筛：音源会混入同名歌曲/翻唱，
	// 先按「歌手匹配优先」排序再截断，用户拿到的 N 首里绝大多数是本人原唱。
	// 大批量再多取 50%，抵消多源去重造成的数量损耗。
	artistOnly := artist != "" && query == ""
	fetchTarget := limit
	if artistOnly {
		fetchTarget = limit * 2
	} else if limit > 30 {
		fetchTarget = limit + limit/2
	}
	if fetchTarget > 300 {
		fetchTarget = 300
	}

	var results []MusicItem

	// Source 1: Search from sources.json (local config)
	sources := readSources()
	for _, source := range sources {
		titleMatch := query == "" || containsIgnoreCase(source.Title, query)
		artistMatch := artist == "" || containsIgnoreCase(source.Artist, artist)
		if titleMatch && artistMatch {
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
			if len(results) >= fetchTarget {
				break
			}
		}
	}

	// Source 2: Search from local files
	localItems := searchLocalMusic(query)
	results = append(results, localItems...)

	// 在线音源补齐到目标条数
	if len(results) < fetchTarget {
		apiResults := searchFromAPI(query, artist, fetchTarget-len(results))
		results = append(results, apiResults...)
	}

	// 歌名+歌手都指定时，按歌手再做一次精准过滤，避免混入其他歌手版本。
	// 只填歌手时不按歌手过滤（音源本身已按歌手搜），避免音源歌手字段写法差异误杀。
	if artist != "" && query != "" {
		filtered := results[:0]
		for _, item := range results {
			if containsIgnoreCase(item.Artist, artist) || containsIgnoreCase(artist, item.Artist) {
				filtered = append(filtered, item)
			}
		}
		results = filtered
	}

	// 只按歌手搜：歌手对得上的排前面（其余翻唱/同名曲排后面，超出条数时被截掉）
	if artistOnly && len(results) > 1 {
		sort.SliceStable(results, func(i, j int) bool {
			return containsIgnoreCase(results[i].Artist, artist) && !containsIgnoreCase(results[j].Artist, artist)
		})
	}

	// Limit total results
	if len(results) > limit {
		results = results[:limit]
	}

	return results
}

// searchLocalMusic searches music from local files
// searchOnlySource 只搜某一个音源（前端搜索框的「音源」筛选）。
// 播放地址仍然统一指向 /stream_live，由它做多源回退与转码，和「全部」一致。
func searchOnlySource(only, query, artist string, limit int) []MusicItem {
	song, singer := strings.TrimSpace(query), strings.TrimSpace(artist)
	if song == "" {
		song, singer = singer, ""
	}
	if song == "" && singer == "" {
		return nil
	}

	out := make([]MusicItem, 0, limit)
	seen := make(map[string]bool, limit)
	add := func(src, title, artist string, duration int) {
		title = strings.TrimSpace(title)
		if title == "" || len(out) >= limit {
			return
		}
		if duration <= 0 {
			duration = cachedDuration(title, artist)
		}
		key := toLower(title) + "\x00" + toLower(strings.TrimSpace(artist))
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, MusicItem{
			Title:        title,
			Artist:       strings.TrimSpace(artist),
			Duration:     duration,
			Source:       src,
			AudioURL:     livePlayURL(title, artist),
			AudioFullURL: livePlayURL(title, artist),
		})
	}

	switch only {
	case "qishui":
		if qishuiEnabled() {
			for _, s := range qishuiSearchLimit(song, singer, limit) {
				add("qishui", s.Name, s.Artist, s.Duration)
			}
		}
	case "kuwo":
		for _, s := range kwSearch(song, singer) {
			add("kuwo", s.Name, s.Artist, s.Duration)
		}
	case "netease":
		for _, s := range neSearchLimit(song, singer, limit) {
			add("netease", s.Name, s.Artist, s.Duration)
		}
	case "qq":
		for _, s := range qqSearchLimit(song, singer, limit) {
			add("qq", s.Name, s.Artist, s.Duration)
		}
	case "kugou":
		for _, s := range kgSearchLimit(song, singer, limit) {
			add("kugou", s.Name, s.Artist, s.Duration)
		}
	case "ext":
		if mp3ApiSearchEnabled() {
			for _, it := range mp3ApiCandidates(strings.TrimSpace(song+" "+singer), singer, limit) {
				add("ext", it.Title, it.Artist, it.Duration)
			}
		}
	}
	return out
}

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
func searchFromAPI(query, artist string, limit int) []MusicItem {
	query = strings.TrimSpace(query)
	artist = strings.TrimSpace(artist)
	if query == "" && artist == "" {
		return nil
	}
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	// 各音源函数内部用 song+" "+singer 拼关键词；只填歌手时把歌手放到 song 位
	song, singer := query, artist
	if song == "" {
		song, singer = artist, ""
	}

	results := make([]MusicItem, 0, limit)
	seen := make(map[string]bool, limit)
	add := func(src, title, artist string, duration int) {
		title = strings.TrimSpace(title)
		if title == "" {
			return
		}
		// 音源给的时长经常是 0；这首歌要是以前转过码，本地 meta.json 里有准的
		if duration <= 0 {
			duration = cachedDuration(title, artist)
		}
		key := toLower(title) + "\x00" + toLower(strings.TrimSpace(artist))
		if seen[key] {
			return
		}
		seen[key] = true
		results = append(results, MusicItem{
			Title:        title,
			Artist:       strings.TrimSpace(artist),
			Duration:     duration,
			Source:       src,
			AudioURL:     livePlayURL(title, artist),
			AudioFullURL: livePlayURL(title, artist),
		})
	}
	remaining := func() int { return limit - len(results) }

	// 外部源（mp3api/YouTube）与国内链路并发跑：正常搜索不增加等待时间，
	// 只有国内搜不到时才把外部结果提到前面。
	// 只填歌手时不查外部源——那等于拿歌手名去 YouTube 乱搜，全是噪声。
	var extCh chan []MusicItem
	if mp3ApiSearchEnabled() && query != "" {
		extCh = make(chan []MusicItem, 1)
		keyword := strings.TrimSpace(song + " " + singer)
		go func() {
			extCh <- mp3ApiCandidates(keyword, singer, 8)
		}()
	}

	// 大批量（歌手搜索常见几十上百首）优先网易云：曲库最全、支持 offset 翻页。
	// 小批量仍走酷我主源，响应更快、原唱命中更高。
	if limit > 30 && srcEnabled("netease") {
		fmt.Printf("[Search] source=netease(bulk) query=%s singer=%s limit=%d\n", song, singer, limit)
		for _, s := range neSearchLimit(song, singer, limit) {
			add("netease", s.Name, s.Artist, s.Duration)
			if remaining() <= 0 {
				break
			}
		}
	}

	// 主源：酷我（播放链路首选，搜索接口实测可用、延迟低）
	if srcEnabled("kuwo") && remaining() > 0 {
		fmt.Printf("[Search] source=kuwo query=%s singer=%s\n", song, singer)
		for _, s := range kwSearch(song, singer) {
			add("kuwo", s.Name, s.Artist, s.Duration)
			if remaining() <= 0 {
				break
			}
		}
	}

	// 兜底 1：网易云直连（无版权过滤，能补酷我搜不到的歌）
	if remaining() > 0 && srcEnabled("netease") {
		fmt.Printf("[Search] source=netease query=%s singer=%s\n", song, singer)
		for _, s := range neSearchLimit(song, singer, remaining()) {
			add("netease", s.Name, s.Artist, s.Duration)
			if remaining() <= 0 {
				break
			}
		}
	}

	// 兜底 2：QQ 音乐
	if remaining() > 0 && srcEnabled("qq") {
		fmt.Printf("[Search] source=qq query=%s singer=%s\n", song, singer)
		for _, s := range qqSearchLimit(song, singer, remaining()) {
			add("qq", s.Name, s.Artist, s.Duration)
			if remaining() <= 0 {
				break
			}
		}
	}

	// 兜底 3：酷狗
	if remaining() > 0 && srcEnabled("kugou") {
		fmt.Printf("[Search] source=kugou query=%s singer=%s\n", song, singer)
		for _, s := range kgSearchLimit(song, singer, remaining()) {
			add("kugou", s.Name, s.Artist, s.Duration)
			if remaining() <= 0 {
				break
			}
		}
	}

	// 兜底 4：汽水音乐（抖音曲库，独家/下架歌常有；未登录只有 60 秒试听，
	// 登录后（/tools 扫码）才是完整版，所以放在国内主源之后的补充位）
	if remaining() > 0 && qishuiEnabled() {
		fmt.Printf("[Search] source=qishui query=%s singer=%s\n", song, singer)
		for _, s := range qishuiSearchLimit(song, singer, remaining()) {
			add("qishui", s.Name, s.Artist, s.Duration)
			if remaining() <= 0 {
				break
			}
		}
	}

	// 外部源结果：与国内链路并发跑，最多等 8 秒（yt-dlp 搜索偶尔要 5~6 秒），
	// 拿不到就算了，不拖死整个搜索。
	//   - 国内结果里没有「歌名+歌手」都对得上、且不是 DJ/翻唱版本的 → 外部源提到最前；
	//   - 已经有正版 → 追加到最后，只当补充版本，不影响原有排序。
	if extCh != nil {
		var ext []MusicItem
		select {
		case ext = <-extCh:
		case <-time.After(8 * time.Second):
			fmt.Printf("[Search] external source timeout, skipped\n")
		}
		if len(ext) > 0 {
			if !domesticHasCleanMatch(results, song, singer) {
				fmt.Printf("[Search] external source promoted %d candidates to head\n", len(ext))
				results = append(ext, results...)
			} else if remaining() > 0 {
				results = append(results, ext...)
			}
		}
	}

	fmt.Printf("[Search] online sources returned %d candidates\n", len(results))
	return results
}

// domesticHasCleanMatch 判断国内结果里有没有「干净的原曲」能顶上。
//
// 只看前几条：如果前三条里有一条歌名对得上、且不是 DJ/抖音/超燃/伴奏 这类
// 二次加工版，就认为国内有这首歌（外部源只当补充）；
// 否则（典型：被禁歌曲，国内只剩一堆翻唱/DJ 版）就把外部源提到最前面。
func domesticHasCleanMatch(items []MusicItem, song, singer string) bool {
	wantTitle := kwNormName(song)
	wantArtist := strings.TrimSpace(singer)
	checked := 0
	for _, it := range items {
		gotTitle := kwNormName(it.Title)
		if gotTitle == "" || wantTitle == "" {
			continue
		}
		if !strings.Contains(gotTitle, wantTitle) && !strings.Contains(wantTitle, gotTitle) {
			continue
		}
		if wantArtist != "" &&
			!containsIgnoreCase(it.Artist, wantArtist) && !containsIgnoreCase(wantArtist, it.Artist) {
			continue
		}
		checked++
		if !isCoverishTitle(it.Title) {
			return true
		}
		if checked >= 3 {
			break
		}
	}
	return false
}

var coverishWords = []string{
	"dj", "版", "翻唱", "cover", "抖音", "硬曲", "超燃", "remix", "电音",
	"伴奏", "纯音乐", "instrumental", "karaoke", "ktv", "变调", "加速", "慢速",
	"女声", "男声", "童声", "合唱", "铃声", "片段", "试听", "混音",
}

func isCoverishTitle(title string) bool {
	low := strings.ToLower(title)
	for _, w := range coverishWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// songTitleMatches 判断一条候选的歌名是否就是要的那首。
// 允许「晴天」与「晴天 (Live)」这类包含关系，但拦住「灰色轨迹」冒充「長城」。
func songTitleMatches(gotTitle, wantSong string) bool {
	got := kwNormName(gotTitle)
	want := kwNormName(wantSong)
	if got == "" || want == "" {
		return got == want
	}
	return got == want || strings.Contains(got, want) || strings.Contains(want, got)
}

// hasDirectMatch 保留给其它调用方：是否已有「歌名+歌手」都对得上的一条
func hasDirectMatch(items []MusicItem, song, singer string) bool {
	wantTitle := kwNormName(song)
	wantArtist := strings.TrimSpace(singer)
	if wantTitle == "" {
		return len(items) > 0
	}
	for _, it := range items {
		gotTitle := kwNormName(it.Title)
		if gotTitle == "" {
			continue
		}
		titleOK := gotTitle == wantTitle ||
			strings.Contains(gotTitle, wantTitle) || strings.Contains(wantTitle, gotTitle)
		if !titleOK {
			continue
		}
		// DJ 版/伴奏/翻唱 这类不算「有这首歌」
		if _, fatal, _ := srcNoiseOf(it.Title, "", song); fatal {
			continue
		}
		if wantArtist == "" {
			return true
		}
		if containsIgnoreCase(it.Artist, wantArtist) || containsIgnoreCase(wantArtist, it.Artist) {
			return true
		}
	}
	return false
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
