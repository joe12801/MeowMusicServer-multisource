package main

// 歌词
//
// 各音源返回的 lyric_url 基本是空的，Web 端一直没歌词可显示。
// 现在的取序：
//   1. LRCLIB（lrclib.net）：免费、无需密钥、直接给带时间轴的 LRC，
//      华语热门曲命中率很高，实测《晴天》一次命中；
//   2. LRCLIB 搜索接口：精确匹配没有时按模糊结果打分挑一条；
//   3. 网易云：cloudsearch 找曲目 ID → /api/song/lyric 取 LRC（顺带 tlyric 翻译）。
//   4. LrcAPI（lyric_lrcapi.go）：Apple Music 逐字歌词 + 酷狗聚合，补前两家都没收录的
//      冷门歌 / OST / 翻唱，只在前两家都没结果时才打，避免多绕一跳。
//
// 带翻译（?trans=1）时优先走网易云，因为只有它给 tlyric；
// 网易云没结果再回落 LRCLIB（无翻译，但至少有时间轴），最后才是 LrcAPI。
//
// 缓存落 files/cache/lyrics/<歌手-歌名>.json，同时存原文与翻译；
// 老版本留下的同名 .lrc 仍可读取，命中后按需升级为 JSON。
//
// 歌词属于「有则更好」：任何一步失败都静默降级，前端显示「暂无歌词」，
// 绝不因为歌词拖慢播放或搜索。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type lyricLine struct {
	Time  float64 `json:"time"` // 秒
	Text  string  `json:"text"`
	Trans string  `json:"trans,omitempty"` // 翻译，没有则省略
}

// lyricResult 一次性把「有没有时间轴」说清楚：
// 纯文本歌词（plainLyrics）没有时间标签，前端只能静态滚动，不做高亮。
type lyricResult struct {
	LRC        string
	Plain      string
	Trans      string // 翻译的 LRC，时间轴与 LRC 大致对齐
	Synced     bool
	Source     string
	TransTried bool // 已经尝试过补翻译，失败就不再反复试
}

// lyricCacheEntry 是磁盘上的缓存结构（老版本是纯 .lrc，没有这个）。
type lyricCacheEntry struct {
	LRC        string `json:"lrc,omitempty"`
	Plain      string `json:"plain,omitempty"`
	Trans      string `json:"trans,omitempty"`
	Synced     bool   `json:"synced"`
	TransTried bool   `json:"trans_tried,omitempty"`
	Source     string `json:"source,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}

var lrcTagRe = regexp.MustCompile(`\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

// 制作信息行（「作词 : 某某」）不是歌词，占着时间轴还会把翻译挤错位，直接丢掉。
var creditLineRe = regexp.MustCompile(`(?i)^\s*(作词|作詞|作曲|编曲|編曲|制作人|製作人|出品人|监制|監製|统筹|統籌|混音|母带|母帶|录音|錄音|和声|和聲|吉他|贝斯|貝斯|鼓|键盘|鍵盤|弦乐|弦樂|词|詞|曲|lyrics?|composed|arranged|produced|written)\s*[:：]`)

// LRC 元信息行（[ti:] [ar:] [al:] [by:] [hash:] [total:] …）不是歌词，
// 纯文本歌词里也常混进来（酷狗导出的尤其多），要过滤掉。
var lrcMetaLineRe = regexp.MustCompile(`^\[[a-zA-Z_]+:.*\]$`)

// 占位歌词：有的音源没词时会给一句「纯音乐，请欣赏」，这不是歌词。
var placeholderLyricRe = regexp.MustCompile(`^(纯音乐[，,、\s]*(请欣赏)?|此歌曲为没有填词的纯音乐.*|纯音乐请欣赏|暂无歌词.*|该歌曲暂无歌词.*)$`)

// isJunkLyricLine 判断一行是不是「不算歌词」的行。
func isJunkLyricLine(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return true
	}
	if lrcMetaLineRe.MatchString(t) {
		return true
	}
	return placeholderLyricRe.MatchString(t)
}

// countLyricContent 数一下有多少行真的算歌词（用于判断「命中了但其实是空壳」）。
func countLyricContent(text string) int {
	n := 0
	for _, raw := range strings.Split(text, "\n") {
		t := strings.TrimSpace(lrcTagRe.ReplaceAllString(strings.TrimRight(raw, "\r"), ""))
		if isJunkLyricLine(t) || creditLineRe.MatchString(t) {
			continue
		}
		n++
	}
	return n
}

var lrclibClient = &http.Client{Timeout: 12 * time.Second}

type lrclibItem struct {
	ID           int64   `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

func lyricCacheFile(song, singer string) string {
	return filepath.Join(lyricCacheRoot, cacheDirName(singer, song)+".json")
}

func legacyLyricCacheFile(song, singer string) string {
	return filepath.Join(lyricCacheRoot, cacheDirName(singer, song)+".lrc")
}

func loadLyricCache(song, singer string) (*lyricResult, bool) {
	if b, err := os.ReadFile(lyricCacheFile(song, singer)); err == nil && len(b) > 0 {
		var e lyricCacheEntry
		if err := json.Unmarshal(b, &e); err == nil {
			r := &lyricResult{
				LRC:        e.LRC,
				Plain:      e.Plain,
				Trans:      e.Trans,
				Synced:     e.Synced,
				Source:     "cache",
				TransTried: e.TransTried,
			}
			if r.LRC != "" || r.Plain != "" {
				return r, true
			}
		}
	}

	// 老版本缓存：纯文本 .lrc
	if b, err := os.ReadFile(legacyLyricCacheFile(song, singer)); err == nil && len(b) > 0 {
		text := string(b)
		synced := strings.Contains(text, "\n[") || strings.HasPrefix(text, "[")
		r := &lyricResult{Source: "cache"}
		if synced {
			r.LRC, r.Synced = text, true
		} else {
			r.Plain = text
		}
		return r, true
	}
	return nil, false
}

func saveLyricCache(song, singer string, r *lyricResult) {
	if r == nil {
		return
	}
	e := lyricCacheEntry{
		LRC:        r.LRC,
		Plain:      r.Plain,
		Trans:      r.Trans,
		Synced:     r.Synced,
		TransTried: r.TransTried,
		Source:     r.Source,
		UpdatedAt:  time.Now().Unix(),
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := writeFileAtomic(lyricCacheFile(song, singer), b, 0o644); err != nil {
		fmt.Printf("[Lyric] 写缓存失败: %v\n", err)
		return
	}
	// 升级成功后清掉旧 txt，避免两份内容打架
	if _, err := os.Stat(legacyLyricCacheFile(song, singer)); err == nil {
		os.Remove(legacyLyricCacheFile(song, singer))
	}
}

// ---------- 「确实没有歌词」短期缓存 ----------

const lyricMissTTL = 12 * time.Hour

// lyricMissFile 放在歌词缓存目录里，前缀 _miss_ 与正常缓存区分开。
func lyricMissFile(song, singer string) string {
	return filepath.Join(lyricCacheRoot, "_miss_"+cacheDirName(singer, song)+".json")
}

type lyricMissEntry struct {
	At int64 `json:"at"`
}

func lyricMissFresh(song, singer string) bool {
	b, err := os.ReadFile(lyricMissFile(song, singer))
	if err != nil || len(b) == 0 {
		return false
	}
	var e lyricMissEntry
	if json.Unmarshal(b, &e) != nil || e.At <= 0 {
		return false
	}
	return time.Since(time.Unix(e.At, 0)) < lyricMissTTL
}

func saveLyricMiss(song, singer string) {
	b, _ := json.Marshal(lyricMissEntry{At: time.Now().Unix()})
	_ = writeFileAtomic(lyricMissFile(song, singer), b, 0o644)
}

// warmPlaylistLyrics 夜间给歌单里的歌预取歌词。
// LrcAPI 冷查询要 5~10 秒，白天第一次打开歌词面板要干等；凌晨先把歌单跑一遍，
// 白天就是读缓存。
func warmPlaylistLyrics(limit int) {
	if limit <= 0 {
		return
	}
	playlistManager.mu.RLock()
	candidates := make([]MusicItem, 0, 128)
	for _, pls := range playlistManager.UserPlaylists {
		for _, pl := range pls {
			candidates = append(candidates, pl.Songs...)
		}
	}
	playlistManager.mu.RUnlock()

	seen := map[string]struct{}{}
	done := 0
	for _, it := range candidates {
		if done >= limit {
			break
		}
		title, artist := strings.TrimSpace(it.Title), strings.TrimSpace(it.Artist)
		if title == "" {
			continue
		}
		key := strings.ToLower(artist + "|" + title)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := loadLyricCache(title, artist); ok {
			continue
		}
		if lyricMissFresh(title, artist) {
			continue
		}
		t0 := time.Now()
		if r, err := fetchLyrics(title, artist, false); err == nil && r != nil {
			done++
			fmt.Printf("[Lyric] 预热 %s - %s（%s，%.1fs）\n", artist, title, r.Source, time.Since(t0).Seconds())
		}
	}
	fmt.Printf("[Lyric] 本轮预热 %d 首\n", done)
}

func lrclibRequest(rawURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0 (https://github.com/joe12801/MeowMusicServer-multisource)")
	req.Header.Set("Accept", "application/json")
	resp, err := lrclibClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("没有收录")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lrclib http %d", resp.StatusCode)
	}
	return readAllLimited(resp, 1<<20)
}

func readAllLimited(resp *http.Response, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("响应过大")
	}
	return body, nil
}

// lrclibGet 精确匹配（lrclib 对 track_name + artist_name 有索引）。
func lrclibGet(song, singer string) (*lrclibItem, error) {
	q := url.Values{}
	q.Set("track_name", song)
	if singer != "" {
		q.Set("artist_name", singer)
	}
	body, err := lrclibRequest("https://lrclib.net/api/get?" + q.Encode())
	if err != nil {
		return nil, err
	}
	var it lrclibItem
	if err := json.Unmarshal(body, &it); err != nil {
		return nil, err
	}
	return &it, nil
}

func lrclibSearch(song, singer string) []lrclibItem {
	q := url.Values{}
	q.Set("track_name", song)
	if singer != "" {
		q.Set("artist_name", singer)
	}
	body, err := lrclibRequest("https://lrclib.net/api/search?" + q.Encode())
	if err != nil {
		return nil
	}
	var list []lrclibItem
	if err := json.Unmarshal(body, &list); err != nil {
		return nil
	}
	return list
}

func scoreLyricItem(it lrclibItem, song, singer string) int {
	score := 0
	title := strings.TrimSpace(it.TrackName)
	if strings.EqualFold(title, strings.TrimSpace(song)) {
		score += 60
	} else if containsIgnoreCase(title, song) || containsIgnoreCase(song, title) {
		score += 30
	} else {
		score -= 40
	}
	if singer != "" && strings.TrimSpace(it.ArtistName) != "" {
		if containsIgnoreCase(it.ArtistName, singer) || containsIgnoreCase(singer, it.ArtistName) {
			score += 60
		} else {
			score -= 40
		}
	}
	if it.SyncedLyrics != "" {
		score += 10
	}
	return score
}

func lyricFromLRCLIB(song, singer string) (*lyricResult, error) {
	if it, err := lrclibGet(song, singer); err == nil && it != nil && !it.Instrumental {
		if r := lyricResultFromItem(*it); r != nil {
			return r, nil
		}
	}

	list := lrclibSearch(song, singer)
	if len(list) == 0 {
		return nil, fmt.Errorf("lrclib 无结果")
	}
	sort.SliceStable(list, func(i, j int) bool {
		return scoreLyricItem(list[i], song, singer) > scoreLyricItem(list[j], song, singer)
	})
	best := list[0]
	if scoreLyricItem(best, song, singer) < 50 {
		return nil, fmt.Errorf("lrclib 无匹配结果")
	}
	if r := lyricResultFromItem(best); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("lrclib 无歌词内容")
}

func lyricResultFromItem(it lrclibItem) *lyricResult {
	if strings.TrimSpace(it.SyncedLyrics) != "" {
		return &lyricResult{LRC: it.SyncedLyrics, Synced: true, Source: "lrclib"}
	}
	if strings.TrimSpace(it.PlainLyrics) != "" {
		return &lyricResult{Plain: it.PlainLyrics, Source: "lrclib"}
	}
	return nil
}

// neFindSongIDs 用「歌名 + 歌手」找候选曲目 ID，按匹配分从高到低。
// 复用播放链路同一套打分：DJ 版/伴奏/现场这类噪声优先排除；
// 如果全被判成噪声（音源标题写法千奇百怪），就退化成按分数排，至少还有得试。
func neFindSongIDs(song, singer string, max int) []int64 {
	cands := neSearchLimit(song, singer, 10)
	if len(cands) == 0 {
		fmt.Printf("[Lyric] 网易云没有搜到 %q - %q 的任何候选\n", song, singer)
		return nil
	}

	type scored struct {
		id    int64
		score int
		fatal bool
	}
	list := make([]scored, 0, len(cands))
	for i := range cands {
		score, fatal := neScoreTrack(&cands[i], song, singer)
		list = append(list, scored{id: cands[i].ID, score: score, fatal: fatal})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].score > list[j].score })

	out := make([]int64, 0, max)
	for _, s := range list {
		if s.fatal {
			continue
		}
		// 分数太低说明搜到的是不相干的东西（歌名对不上、歌手也对不上），
		// 这种宁可判「没歌词」，也不能把别人的歌词贴上去。
		if s.score < 30 {
			continue
		}
		out = append(out, s.id)
		if len(out) >= max {
			break
		}
	}
	if len(out) == 0 {
		if len(list) == 0 || list[0].score < 30 {
			return nil
		}
		for _, s := range list {
			if s.fatal || s.score < 30 {
				continue
			}
			out = append(out, s.id)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

// neFetchLRC 取歌词与翻译。tv=-1 时接口会一并返回 tlyric（翻译版）。
func neFetchLRC(id int64) (lrc, trans string, err error) {
	api := fmt.Sprintf("https://music.163.com/api/song/lyric?id=%d&lv=1&kv=1&tv=-1", id)
	resp, err := neGet(api, true)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("lyric api http %d", resp.StatusCode)
	}

	var r struct {
		Lrc struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
		Tlyric struct {
			Lyric string `json:"lyric"`
		} `json:"tlyric"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", err
	}
	lrc = strings.TrimSpace(r.Lrc.Lyric)
	trans = strings.TrimSpace(r.Tlyric.Lyric)
	if lrc == "" {
		return "", "", fmt.Errorf("该曲目暂无歌词")
	}
	return lrc, trans, nil
}

func lyricFromNetease(song, singer string) (*lyricResult, error) {
	ids := neFindSongIDs(song, singer, 4)
	if len(ids) == 0 {
		return nil, fmt.Errorf("网易云无候选")
	}
	var lastErr error
	for _, id := range ids {
		lrc, trans, err := neFetchLRC(id)
		if err != nil {
			lastErr = err
			continue
		}
		return &lyricResult{LRC: lrc, Trans: trans, Synced: true, Source: "netease"}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("网易云无歌词")
	}
	return nil, lastErr
}

// fetchLyrics 依次尝试各歌词源，命中即落盘。
func fetchLyrics(song, singer string, wantTrans bool) (*lyricResult, error) {
	song = strings.TrimSpace(song)
	singer = strings.TrimSpace(singer)
	if song == "" {
		return nil, fmt.Errorf("缺少 song 参数")
	}

	// 「确实没歌词」也记一笔：LrcAPI 冷查询要 5~10 秒，一首本来就无词的歌
	// 每播一次都去问三遍太浪费，12 小时内直接告诉前端没有。
	if lyricMissFresh(song, singer) {
		return nil, fmt.Errorf("未找到歌词（已缓存，稍后再试）")
	}

	if cached, ok := loadLyricCache(song, singer); ok {
		// 缓存里没有翻译而这次要翻译：补一次，失败就记下来别再反复试
		if wantTrans && cached.Trans == "" && !cached.TransTried {
			if ne, err := lyricFromNetease(song, singer); err == nil && ne != nil && ne.Trans != "" {
				cached.Trans = ne.Trans
			}
			cached.TransTried = true
			saveLyricCache(song, singer, cached)
		}
		return cached, nil
	}

	var result *lyricResult
	var lrErr, neErr, lrcErr error

	if wantTrans {
		result, neErr = lyricFromNetease(song, singer)
		if result == nil {
			result, lrErr = lyricFromLRCLIB(song, singer)
		}
	} else {
		result, lrErr = lyricFromLRCLIB(song, singer)
		if result == nil {
			result, neErr = lyricFromNetease(song, singer)
		}
	}
	// 前两家都没有（冷门歌、OST、翻唱），最后补一路 LrcAPI
	if result == nil {
		result, lrcErr = lyricFromLrcAPI(song, singer)
	}

	if result == nil {
		detail := "未找到歌词"
		if lrErr != nil {
			detail = "未找到歌词（" + lrErr.Error() + "）"
		}
		if neErr != nil {
			detail += "；" + neErr.Error()
		}
		if lrcErr != nil {
			detail += "；" + lrcErr.Error()
		}
		recordSourceResult("lyric", false, 0, detail)
		saveLyricMiss(song, singer)
		return nil, fmt.Errorf("%s", detail)
	}

	text := result.LRC
	if !result.Synced {
		text = result.Plain
	}
	if len(text) > 256*1024 { // 防御：异常超长内容不入缓存
		return nil, fmt.Errorf("歌词内容异常")
	}
	if result.Trans != "" && len(result.Trans) > 256*1024 {
		result.Trans = ""
	}
	result.TransTried = wantTrans || result.Trans != ""
	saveLyricCache(song, singer, result)
	recordSourceResult("lyric", true, 0, "")
	invalidateCacheSnapshot()
	return result, nil
}

// parseLRC 把 LRC 拆成按时间排序的行，过滤掉 [ar:] [ti:] 这类元信息。
func parseLRC(lrc string) []lyricLine {
	lines := make([]lyricLine, 0, 80)
	for _, raw := range strings.Split(lrc, "\n") {
		raw = strings.TrimRight(raw, "\r")
		matches := lrcTagRe.FindAllStringSubmatch(raw, -1)
		if len(matches) == 0 {
			continue
		}
		text := strings.TrimSpace(lrcTagRe.ReplaceAllString(raw, ""))
		if isJunkLyricLine(text) || creditLineRe.MatchString(text) {
			continue
		}
		for _, m := range matches {
			min, _ := strconv.Atoi(m[1])
			sec, _ := strconv.Atoi(m[2])
			ms := 0
			if m[3] != "" {
				frac := m[3]
				for len(frac) < 3 {
					frac += "0"
				}
				ms, _ = strconv.Atoi(frac[:3])
			}
			lines = append(lines, lyricLine{
				Time: float64(min*60+sec) + float64(ms)/1000.0,
				Text: text,
			})
		}
	}
	// 多标签行会产生多条，按时间排一下（LRC 通常已有序，这里防御异常文件）
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time < lines[j].Time })
	return lines
}

// plainToLines 把纯文本歌词按行拆开，时间统一为 -1（前端不做高亮）。
func plainToLines(plain string) []lyricLine {
	out := make([]lyricLine, 0, 60)
	for _, raw := range strings.Split(plain, "\n") {
		t := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if isJunkLyricLine(t) {
			continue
		}
		out = append(out, lyricLine{Time: -1, Text: t})
	}
	return out
}

// mergeTrans 把翻译按时间贴到原文行上。
//
// 网易云的 tlyric 通常与 lrc 逐行对应，但开头那几行「作词/作曲」翻译版里
// 没有，直接按时间匹配会整体错开一行（歌词全串位）。所以：
//   - 两边行数相同时按顺序对齐（最可靠）；
//   - 行数不同再退回时间匹配，容差 0.5 秒，差太多宁可不贴。
func mergeTrans(lines []lyricLine, transLRC string) bool {
	if transLRC == "" || len(lines) == 0 {
		return false
	}
	t := parseLRC(transLRC)
	if len(t) == 0 {
		return false
	}

	if len(t) == len(lines) {
		hit := 0
		for i := range lines {
			if lines[i].Time < 0 {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(t[i].Text), strings.TrimSpace(lines[i].Text)) {
				continue
			}
			lines[i].Trans = t[i].Text
			hit++
		}
		return hit > 0
	}

	j, hit := 0, 0
	for i := range lines {
		if lines[i].Time < 0 {
			continue
		}
		for j < len(t) && t[j].Time < lines[i].Time-0.5 {
			j++
		}
		if j < len(t) && math.Abs(t[j].Time-lines[i].Time) <= 0.5 {
			// 翻译和原文一模一样时不用重复显示
			if !strings.EqualFold(strings.TrimSpace(t[j].Text), strings.TrimSpace(lines[i].Text)) {
				lines[i].Trans = t[j].Text
				hit++
			}
		}
	}
	return hit > 0
}

// HandleLyric 提供歌词：默认 JSON（含解析好的时间轴），?format=lrc 直接给纯文本。
// ?trans=1 时优先取带翻译的版本，JSON 里每行会多一个 trans 字段。
func HandleLyric(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")

	q := r.URL.Query()
	song := q.Get("song")
	singer := q.Get("singer")
	if strings.TrimSpace(song) == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "缺少 song 参数"})
		return
	}

	wantTrans := false
	switch strings.ToLower(strings.TrimSpace(q.Get("trans"))) {
	case "1", "true", "yes", "on":
		wantTrans = true
	}

	// 人工纠错的歌词优先于任何自动源
	res := lookupCustomLyric(song, singer)
	var err error
	if res == nil {
		res, err = fetchLyrics(song, singer, wantTrans)
	}

	if strings.EqualFold(q.Get("format"), "lrc") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err != nil || res == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if res.Synced {
			w.Write([]byte(res.LRC))
		} else {
			w.Write([]byte(res.Plain))
		}
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]interface{}{
		"song":   song,
		"artist": singer,
	}
	if err != nil || res == nil {
		resp["success"] = false
		resp["message"] = errMessage(err)
		resp["lrc"] = ""
		resp["lines"] = []lyricLine{}
		resp["synced"] = false
		resp["has_trans"] = false
	} else {
		lines := parseLRC(res.LRC)
		if !res.Synced {
			lines = plainToLines(res.Plain)
		}
		hasTrans := mergeTrans(lines, res.Trans)
		resp["success"] = true
		resp["source"] = res.Source
		resp["synced"] = res.Synced
		resp["has_trans"] = hasTrans
		resp["lrc"] = res.LRC
		resp["lines"] = lines
	}
	json.NewEncoder(w).Encode(resp)
}

func errMessage(err error) string {
	if err == nil {
		return "未找到歌词"
	}
	return err.Error()
}
