package main

// mp3api 搜索接入（mp3.994938.xyz /api/search）
//
// 背景：国内音源（酷我/网易云/QQ/酷狗）对「被下架/被禁」的曲子基本是空的——
// 例如 Beyond《长城》：各家搜「长城 Beyond」返回的全是 Beyond 别的歌，
// 原曲一条都没有。而 mp3api 走 YouTube，曲库覆盖这类歌完全没问。
//
// 所以这里把 mp3api 当「补位源」而不是主源：
//   - 国内音源里已经有「歌名+歌手都对得上」的结果时，外部源只排在最后补数量；
//   - 一条都对不上时（等于国内搜不到），外部源结果直接放到最前面，
//     否则会被国内那些同歌手别的歌顶掉。
//
// 播放不直接用 mp3api 地址（会把 api_key 暴露到浏览器/设备），
// 统一走本站代理 /api/ext_audio?id=<video_id>，KEY 只在服务端使用。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type mp3ApiVideo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Channel    string `json:"channel"`
	Duration   int    `json:"duration"`
	Thumbnail  string `json:"thumbnail"`
	WebpageURL string `json:"webpage_url"`
	ViewCount  int64  `json:"view_count"`
}

type mp3ApiSearchResp struct {
	Query   string        `json:"query"`
	Count   int           `json:"count"`
	Results []mp3ApiVideo `json:"results"`
}

// 外部源候选里「明显不是一首歌」的词
var extNonMusicWords = []string{
	"纪录片", "纪录", "讲座", "访谈", "综艺", "新闻", "教程", "教学", "解说",
	"相声", "小品", "游戏", "reaction", "测评", "盘点", "合集", "混剪",
	"预告", "花絮", "采访", "发布会", "直播回放", "电视剧", "电影",
	"攻略", "旅游", "游览", "路线", "打卡", "风景", "匠人", "探访",
	"讲解", "美食", "民宿", "vlog", "旅行", "自驾", "徒步", "考古", "文物",
	"抖音", "热歌", "神曲", "洗脑", "串烧", "解讀", "解读", "深度解析",
}

// 标题/频道里出现这些词，才认为它「像一首歌」（避免把长城游记当成歌）
var extMusicSignals = []string{
	"《", "「", "official", "mv", "m/v", "audio", "lyrics", "歌词",
	"live", "现场", "演唱会", "music", "音乐", "原唱", "主题曲", "插曲",
	"专辑", "專輯", "cover", "remix", "instrumental", "版", "唱片", "天碟",
}

// 标题里的「版本装饰」：括号里出现这些词就整段丢掉
var extDecorationWords = []string{
	"official", "mv", "m/v", "music video", "高清", "hd", "4k", "8k", "1080p",
	"live", "现场", "演唱会", "歌词", "lyric", "完整版", "音频", "audio",
	"中字", "字幕", "翻唱", "cover", "dj", "remix", "伴奏", "纯音乐",
	"instrumental", "karaoke", "超清", "无损", "试听",
}

// 标题里不带括号的版本标注，直接抹掉（否则会出现「長城 Official MV」）
var extDecorationPhrases = []string{
	"official music video", "official video", "official audio", "official mv", "official hd",
	"music video", "lyric video", "lyrics video", "audio only", "m/v",
	"官方mv", "官方版", "中文字幕", "完整版", "高清版", "超清", "无损音质",
}

var extSepRe = regexp.MustCompile(`\s*[-–—|｜]\s*`)
var extBracketRe = regexp.MustCompile(`[《》]`)
var extGroupRe = regexp.MustCompile(`[（(【\[「]([^）)】\]」]*)[）)】\]」]`)

// mp3ApiSearchEnabled 外部源是否可用：必须配了 KEY 且未被 MUSIC_SOURCES 关掉
func mp3ApiSearchEnabled() bool {
	return mp3ApiKey() != "" && srcEnabled("mp3api")
}

// mp3ApiSearchVideos 调 mp3api 的搜索接口（只搜不下载，1~2 秒返回）
func mp3ApiSearchVideos(keyword string, limit int) []mp3ApiVideo {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	target := fmt.Sprintf("%s/api/search?q=%s&limit=%d&api_key=%s",
		mp3ApiBase(), url.QueryEscape(keyword), limit, url.QueryEscape(mp3ApiKey()))

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[mp3api-search] %q request error: %v\n", keyword, err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		fmt.Printf("[mp3api-search] %q http %d: %s\n", keyword, resp.StatusCode, strings.TrimSpace(string(body)))
		return nil
	}
	var out mp3ApiSearchResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		fmt.Printf("[mp3api-search] %q json error: %v\n", keyword, err)
		return nil
	}
	fmt.Printf("[mp3api-search] %q -> %d hits\n", keyword, len(out.Results))
	return out.Results
}

// extAudioURL 本站代理地址（相对路径，HandleSearch 会补成绝对地址）
func extAudioURL(videoID string) string {
	return "/api/ext_audio?id=" + url.QueryEscape(videoID)
}

// mp3ApiCandidates 把 YouTube 结果转成「歌名 + 歌手」候选，并做音乐性过滤。
// 只保留像歌的结果（时长合理、标题/频道像音乐、不含纪录片类词）。
func mp3ApiCandidates(keyword, wantArtist string, limit int) []MusicItem {
	videos := mp3ApiSearchVideos(keyword, 20)
	if len(videos) == 0 {
		return nil
	}

	type scored struct {
		item  MusicItem
		score int
		order int
	}
	var list []scored

	for i, v := range videos {
		if v.ID == "" || strings.TrimSpace(v.Title) == "" {
			continue
		}
		title, artist := extParseTitle(v.Title, v.Channel, wantArtist, keyword)
		if title == "" {
			continue
		}

		lowAll := strings.ToLower(v.Title + " " + v.Channel)
		// 指定了歌手时，标题/频道里必须出现这个歌手，否则是别人的版本
		if wantArtist != "" && !strings.Contains(lowAll, strings.ToLower(strings.TrimSpace(wantArtist))) {
			continue
		}
		// 没有指定歌手时，必须能找到「像首歌」的痕迹，否则会把游记/攻略也收进来
		if wantArtist == "" {
			musicLike := false
			for _, s := range extMusicSignals {
				if strings.Contains(lowAll, s) {
					musicLike = true
					break
				}
			}
			if !musicLike {
				continue
			}
		}
		// 歌曲一般 1~10 分钟，超长的基本是纪录片/合集
		if v.Duration > 0 && v.Duration > 600 {
			continue
		}
		score := 0
		if v.Duration >= 45 && v.Duration <= 900 {
			score += 2
		}
		if v.Duration > 1200 {
			score -= 3
		}
		if wantArtist != "" && strings.Contains(lowAll, strings.ToLower(wantArtist)) {
			score += 3
		}
		if strings.Contains(strings.ToLower(v.Channel), "music") ||
			strings.Contains(v.Channel, "音乐") || strings.Contains(v.Channel, "唱片") ||
			strings.Contains(v.Channel, "天碟") || strings.Contains(strings.ToLower(v.Channel), "topic") {
			score += 1
		}
		for _, w := range extNonMusicWords {
			if strings.Contains(lowAll, w) {
				score -= 4
				break
			}
		}
		// 「官摄/MV」这类视频仍然是原曲，给一点加分
		if strings.Contains(strings.ToLower(v.Title), "official") || strings.Contains(v.Title, "官方") {
			score += 1
		}
		if score < 2 {
			continue
		}

		list = append(list, scored{
			item: MusicItem{
				Title:        title,
				Artist:       artist,
				Duration:     v.Duration,
				AudioURL:     extAudioURL(v.ID),
				AudioFullURL: extAudioURL(v.ID),
				CoverURL:     v.Thumbnail,
				Source:       "ext",
			},
			score: score,
			order: i,
		})
	}

	sort.SliceStable(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].order < list[j].order
	})

	if limit <= 0 || limit > len(list) {
		limit = len(list)
	}
	out := make([]MusicItem, 0, limit)
	seen := make(map[string]bool, limit)
	for _, s := range list {
		key := strings.ToLower(s.item.Title) + "\x00" + strings.ToLower(s.item.Artist)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s.item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// extParseTitle 从 YouTube 标题里拆出「歌名 + 歌手」。
// 优先用搜索时已知的歌手做对照，其次按「歌手 - 歌名」的常见写法猜。
func extParseTitle(raw, channel, wantArtist, keyword string) (title, artist string) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", ""
	}
	// 《》只保留内容
	t = extBracketRe.ReplaceAllString(t, " ")
	// 括号装饰：含 official/live/歌词 等关键词的整段丢掉，其余只去括号。
	// 注意括号是中文全角，必须按 rune 切，按字节切会把汉字截成乱码。
	t = extGroupRe.ReplaceAllStringFunc(t, func(m string) string {
		rs := []rune(m)
		if len(rs) < 2 {
			return " "
		}
		inner := string(rs[1 : len(rs)-1])
		lowInner := strings.ToLower(inner)
		for _, w := range extDecorationWords {
			if strings.Contains(lowInner, w) {
				return " "
			}
		}
		return " " + inner + " "
	})
	// 不在括号里的「Official MV / 高清 / 完整版」也要抹掉
	t = stripExtDecorationPhrases(t)
	t = strings.Join(strings.Fields(t), " ")

	parts := extSepRe.Split(t, -1)
	if len(parts) >= 2 {
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(strings.Join(parts[1:], " - "))
		la := strings.ToLower(left)
		ra := strings.ToLower(right)

		if wantArtist != "" {
			w := strings.ToLower(strings.TrimSpace(wantArtist))
			if strings.Contains(la, w) || strings.Contains(ra, w) {
				if strings.Contains(la, w) && !strings.Contains(ra, w) {
					return cleanExtName(right), strings.TrimSpace(wantArtist)
				}
				if strings.Contains(ra, w) && !strings.Contains(la, w) {
					return cleanExtName(left), strings.TrimSpace(wantArtist)
				}
			}
		}
		// 「歌手 - 歌名」：左边短、右边长，基本就是这个格式
		if n := len([]rune(left)); n >= 2 && n <= 22 {
			return cleanExtName(right), cleanExtName(left)
		}
	}

	// 没有分隔符：标题以歌手名开头的话，把歌手剥掉
	if wantArtist != "" {
		w := strings.TrimSpace(wantArtist)
		if strings.HasPrefix(strings.ToLower(t), strings.ToLower(w)) {
			rest := strings.TrimSpace(t[len(w):])
			rest = strings.TrimLeft(rest, "-–—:|｜ ")
			if rest != "" {
				return cleanExtName(rest), w
			}
		}
	}

	return cleanExtName(t), cleanExtChannel(channel, wantArtist)
}

// stripExtDecorationPhrases 抹掉标题里不在括号中的版本标注词组
func stripExtDecorationPhrases(t string) string {
	low := strings.ToLower(t)
	for _, p := range extDecorationPhrases {
		for {
			i := strings.Index(low, p)
			if i < 0 {
				break
			}
			t = t[:i] + " " + t[i+len(p):]
			low = strings.ToLower(t)
		}
	}
	return t
}

func cleanExtName(s string) string {	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " -–—|｜:：,，.。")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
}

func cleanExtChannel(ch, wantArtist string) string {
	if a := strings.TrimSpace(wantArtist); a != "" {
		return a
	}
	ch = strings.TrimSpace(ch)
	ch = strings.TrimSuffix(ch, " - Topic")
	for _, w := range []string{"官方频道", "官方", "Official", "MV", "音乐", "Music"} {
		ch = strings.TrimSpace(strings.ReplaceAll(ch, w, ""))
	}
	if ch == "" {
		return "外部源"
	}
	if r := []rune(ch); len(r) > 24 {
		ch = string(r[:24])
	}
	return ch
}

// HandleExtAudio 外部源音频代理：浏览器/设备只看到本站地址，api_key 不外泄。
// 支持 Range 透传（拖进度条、ESP32 边下边播都要用）。
func HandleExtAudio(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if id == "" && q == "" {
		http.Error(w, "missing id or q", http.StatusBadRequest)
		return
	}
	key := mp3ApiKey()
	if key == "" {
		http.Error(w, "external source not configured", http.StatusServiceUnavailable)
		return
	}

	target := mp3ApiBase() + "/api/music?"
	if id != "" {
		target += "video_id=" + url.QueryEscape(id)
	} else {
		target += "q=" + url.QueryEscape(q)
	}
	target += "&api_key=" + url.QueryEscape(key)

	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0")

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[ext_audio] id=%s error: %v\n", id, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 透传决定播放行为的关键头
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "audio/mpeg")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
