package main

// GD Studio 公益音乐 API 音源适配
// 接口文档: https://music-api.gdstudio.xyz/api.php
// 支持 types=search / url / pic / lyric
// 实测可用 source: netease（华语翻唱多、原唱常缺）、joox（中文原唱覆盖较好）
// 不可用: kuwo / tencent / migu / kugou（返回 source is not supported）

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const gdStudioAPI = "https://music-api.gdstudio.xyz/api.php"

// 音源优先级：joox 的华语原唱比例明显高于 netease，且翻唱少
var gdSourceOrder = []string{"joox", "netease"}

type gdTrack struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Artist  []string `json:"artist"`
	Album   string   `json:"album"`
	PicID   string   `json:"pic_id"`
	URLID   string   `json:"url_id"`
	LyricID string   `json:"lyric_id"`
	Source  string   `json:"source"`
	score   int      // 内部排序用，不参与 JSON
}

type gdURLResp struct {
	URL  string `json:"url"`
	BR   int    `json:"br"`
	Size int    `json:"size"`
}

var gdHttpClient = &http.Client{Timeout: 8 * time.Second}

func gdFetch(params url.Values) ([]byte, error) {
	// gdstudio 有并发限流：同时打 4 个请求会返回 503。单请求重试一次即可恢复
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(600 * time.Millisecond)
		}
		req, err := http.NewRequest("GET", gdStudioAPI+"?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
		req.Header.Set("Accept", "application/json")
		resp, err := gdHttpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil {
			return body, nil
		}
		if readErr != nil {
			lastErr = readErr
		} else {
			lastErr = fmt.Errorf("http status %d", resp.StatusCode)
		}
	}
	return nil, lastErr
}

// ---------- 选曲打分 ----------

// 翻唱/二创的典型标记。命中即重罚，除非用户搜索词本身包含该词
var coverMarkers = []string{
	"cover", "翻唱", "原唱", "翻自", "伴奏", "伴唱", "karaoke", "instrumental",
	"remix", "dj版", "女声", "男声", "童声", "童音", "钢琴", "吉他", "古筝",
	"纯音乐", "慢速", "加速", "降调", "升调", "铃声", "铃声版",
	"live", "现场", "演唱会", "0.8x", "1.2x", "片段", "试听",
	"治愈", "伤感", "烟嗓", "深情", "甜妹", "慢摇", "蹦迪", "喊麦",
	"抖音", "快手", "热播", "热搜", "神曲", "魔改", "串烧", "合集",
}

// 歌名是否带括号后缀。原版歌曲名通常不带；带括号的多是二创/版本
func gdHasBracket(s string) bool {
	return strings.ContainsAny(s, "(（[【")
}


// 归一化：去括号内容、去空格、转小写
func gdNorm(s string) string {
	s = strings.TrimSpace(s)
	// 去掉中英文括号及其内容
	for _, pair := range [][2]string{{"(", ")"}, {"（", "）"}, {"[", "]"}, {"【", "】"}} {
		for {
			i := strings.Index(s, pair[0])
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], pair[1])
			if j < 0 {
				s = s[:i]
				break
			}
			s = s[:i] + s[i+j+len(pair[1]):]
		}
	}
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

// 字符重合率：want 里有多少字符出现在 got 里。
// 用来兜住繁简差异（周杰伦 / 周杰倫 重合 1/3，而 RyaVocal 重合 0）
func gdOverlap(want, got string) float64 {
	if want == "" {
		return 0
	}
	gr := []rune(got)
	hits := 0
	total := 0
	for _, w := range []rune(want) {
		total++
		for _, g := range gr {
			if w == g {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(total)
}

func gdScoreTrack(t *gdTrack, song, singer string) int {
	score := 0
	wantSong := gdNorm(song)
	wantSinger := gdNorm(singer)
	gotName := gdNorm(t.Name)
	gotArtist := ""
	if len(t.Artist) > 0 {
		gotArtist = gdNorm(strings.Join(t.Artist, "/"))
	}

	// 1) 歌名匹配度
	switch {
	case gotName == wantSong:
		score += 100 // 完全一致，通常是原版
	case strings.HasPrefix(gotName, wantSong):
		score += 45 // "晴天xxx" 这种带后缀的
	case strings.Contains(gotName, wantSong):
		score += 25
	case strings.Contains(wantSong, gotName) && gotName != "":
		score += 10
	}

	// 2) 歌名里的翻唱标记惩罚（用户自己搜的词不罚）
	rawLower := strings.ToLower(t.Name)
	for _, m := range coverMarkers {
		if strings.Contains(rawLower, m) && !strings.Contains(strings.ToLower(song), m) {
			score -= 85
			break
		}
	}

	// 2b) 带括号后缀的重罚：原版通常就叫「晴天」，而「晴天(治愈版)」是二创。
	// 这一步是关键——gdNorm() 会把括号内容删掉，光靠名字归一化区分不出来。
	if gdHasBracket(t.Name) && !gdHasBracket(song) {
		score -= 60
	}

	// 3) 歌手匹配度
	if wantSinger != "" && gotArtist != "" {
		switch {
		case gotArtist == wantSinger:
			score += 60
		case strings.Contains(gotArtist, wantSinger) || strings.Contains(wantSinger, gotArtist):
			score += 50
		default:
			ov := gdOverlap(wantSinger, gotArtist)
			if ov >= 0.5 {
				score += 40
			} else if ov > 0 {
				score += int(ov * 30)
			}
		}
	}

	// 4) 专辑是原版专辑的加分（晴天 → 叶惠美）
	if t.Album != "" && gotName == wantSong && !strings.Contains(strings.ToLower(t.Album), "翻唱") {
		score += 5
	}

	return score
}

// gdRankTracks 按贴合度排序，返回分数 >= minScore 的候选
func gdRankTracks(tracks []gdTrack, song, singer string, minScore int) []gdTrack {
	out := make([]gdTrack, 0, len(tracks))
	for _, t := range tracks {
		t.score = gdScoreTrack(&t, song, singer)
		if t.score >= minScore {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// ---------- 资源解析 ----------

// gdResolveAudioURL 用 track 的 url_id 换取可播放的直链。
// brs 按优先级给码率档：320 最均衡；ESP32 端最终转 24kHz 单声道，再高没意义。
func gdResolveAudioURL(t *gdTrack, brs []string) string {
	id := t.URLID
	if id == "" {
		id = t.ID
	}
	for _, br := range brs {
		p := url.Values{}
		p.Set("types", "url")
		p.Set("source", t.Source)
		p.Set("id", id)
		p.Set("br", br)
		body, err := gdFetch(p)
		if err != nil {
			fmt.Printf("[GDStudio] url(br=%s) error: %v\n", br, err)
			continue
		}
		var r gdURLResp
		if err := json.Unmarshal(body, &r); err != nil {
			continue
		}
		if strings.HasPrefix(r.URL, "http") {
			fmt.Printf("[GDStudio] resolved %s - %s @%dkbps (%dMB)\n",
				t.Name, strings.Join(t.Artist, "/"), r.BR, r.Size/1024/1024)
			return r.URL
		}
	}
	return ""
}

// gdResolveLyric 取回 LRC 文本
func gdResolveLyric(t *gdTrack) string {
	id := t.LyricID
	if id == "" {
		id = t.ID
	}
	p := url.Values{}
	p.Set("types", "lyric")
	p.Set("source", t.Source)
	p.Set("id", id)
	body, err := gdFetch(p)
	if err != nil {
		return ""
	}
	var r struct {
		Lyric string `json:"lyric"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	return r.Lyric
}

// gdResolveCover 取回封面直链
func gdResolveCover(t *gdTrack) string {
	if t.PicID == "" {
		return ""
	}
	p := url.Values{}
	p.Set("types", "pic")
	p.Set("source", t.Source)
	p.Set("id", t.PicID)
	p.Set("size", "300")
	body, err := gdFetch(p)
	if err != nil {
		return ""
	}
	var r struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	return r.URL
}

// ---------- 对外入口 ----------

type gdCand struct {
	track gdTrack
	url   string
}

// gdResolveBest 并发向候选换直链（并发上限 2），按分数择优。
// gdstudio 免费配额很低：串行逐个试 + 重试会把配额耗光，导致后面整批失败，
// 所以这里只打两个请求（每源一个最高分候选），失败的源直接放弃。
func gdResolveBest(queue []gdTrack, limit int) *gdCand {
	if len(queue) == 0 {
		return nil
	}
	if len(queue) > limit {
		queue = queue[:limit]
	}

	ch := make(chan gdCand, len(queue))
	for _, t := range queue {
		go func(t gdTrack) {
			ch <- gdCand{track: t, url: gdResolveAudioURL(&t, []string{"320"})}
		}(t)
	}

	var best *gdCand
	deadline := time.After(15 * time.Second)
	for i := 0; i < len(queue); i++ {
		select {
		case c := <-ch:
			if c.url != "" && (best == nil || c.track.score > best.track.score) {
				cc := c
				best = &cc
			}
		case <-deadline:
			return best
		}
	}
	return best
}

// GDStudioSearch 搜索并返回可直接播放的 MusicItem
func GDStudioSearch(song, singer string) MusicItem {
	keyword := strings.TrimSpace(song)
	if keyword == "" {
		return MusicItem{}
	}
	if singer != "" {
		keyword = keyword + " " + strings.TrimSpace(singer)
	}

	// 1) 各音源各搜一遍，合并候选池后统一打分
	var pool []gdTrack
	for _, src := range gdSourceOrder {
		p := url.Values{}
		p.Set("types", "search")
		p.Set("source", src)
		p.Set("name", keyword)
		p.Set("count", "15")
		p.Set("pages", "1")

		body, err := gdFetch(p)
		if err != nil {
			fmt.Printf("[GDStudio] search(%s) error: %v\n", src, err)
			continue
		}
		var tracks []gdTrack
		if err := json.Unmarshal(body, &tracks); err != nil {
			fmt.Printf("[GDStudio] search(%s) parse error: %v\n", src, err)
			continue
		}
		for i := range tracks {
			tracks[i].Source = src
		}
		fmt.Printf("[GDStudio] search(%s) %q -> %d hits\n", src, keyword, len(tracks))
		pool = append(pool, tracks...)
	}

	if len(pool) == 0 {
		fmt.Printf("[GDStudio] no result for %q\n", keyword)
		return MusicItem{}
	}

	// 2) 按音源分组排序，再交错拼接。
	// 不能混池取前 N：某个源的同名版本会霸占名额（夜曲在 joox 有 3 个同分版本，
	// 且都取不到链接），交错才能保证另一个源有机会顶上。
	var queue []gdTrack
	bySrc := map[string][]gdTrack{}
	for _, t := range pool {
		bySrc[t.Source] = append(bySrc[t.Source], t)
	}
	for _, src := range gdSourceOrder {
		ranked := gdRankTracks(bySrc[src], song, singer, -30)
		bySrc[src] = ranked
	}
	for round := 0; round < 6; round++ {
		for _, src := range gdSourceOrder {
			if round < len(bySrc[src]) {
				queue = append(queue, bySrc[src][round])
			}
		}
	}
	// 全部被分数阈值筛掉时，退回原始顺序碰运气
	if len(queue) == 0 {
		queue = pool
	}

	// 3) 按分数逐个换直链
	if len(queue) > 4 {
		for i := 0; i < 4; i++ {
			fmt.Printf("[GDStudio] cand #%d score=%d %q - %q (%s)\n",
				i+1, queue[i].score, queue[i].Name, strings.Join(queue[i].Artist, "/"), queue[i].Source)
		}
	}
	best := gdResolveBest(queue, 2)
	if best == nil {
		fmt.Printf("[GDStudio] no playable source for %q\n", keyword)
		return MusicItem{}
	}
	t := best.track
	item := MusicItem{
		Title:        t.Name,
		Artist:       strings.Join(t.Artist, "/"),
		AudioURL:     best.url,
		AudioFullURL: best.url,
		CoverURL:     gdResolveCover(&t),
		Duration:     0,
	}
	fmt.Printf("[GDStudio] HIT %q by %q from %s (score=%d)\n",
		item.Title, item.Artist, t.Source, t.score)
	return item
}
