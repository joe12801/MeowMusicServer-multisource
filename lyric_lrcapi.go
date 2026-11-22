package main

// 歌词源：LrcAPI（https://github.com/hisatri/lrcapi）
//
// 为什么再加一路：
//   LRCLIB 对欧美曲目准，网易云对华语准，但两边都有空白区（冷门歌、翻唱、
//   动画 OST、被下架的版本；纯音乐更是双双没结果）。LrcAPI 背后是
//   Apple Music 的逐字歌词 + 酷狗聚合，和上面两家的曲库交集小得多，
//   等于用一路新接口把「暂无歌词」的坑补上。
//
// 接口（新版 JSON，自带时间轴和匹配分）：
//   GET {base}/jsonapi?title=晴天&artist=周杰伦&limit=6
//   -> [{"title":"晴天","artist":"周杰伦","album":"叶惠美",
//        "lrc":"[00:29.188]故事的小黃花","lrc_ttml":"<tt .../>",
//        "score":87.16,"duration":269.7}, ...]
//
// 坑：它的模糊匹配非常「积极」——搜一首根本不存在的歌，它也会把歌名里
// 含相同字眼的另一首歌塞回来（实测能给出完全不相干的曲子）。所以这里
// 必须自己复核「歌名 + 歌手」，宁可返回「没找到」也不能张冠李戴。
//
// 配置（都能在 /tools 页面改，即时生效）：
//   LRCAPI_BASE      默认 https://api.lrc.cx（公共实例）；自建填 http://lrcapi:28883
//   LRCAPI_AUTH      自建实例设了 --auth 时填同一个值（放到 Authorization 头）
//   LRCAPI_ENABLED   0 = 彻底关掉这一路
// 优先级：环境变量 > 页面保存的值 > 默认值。

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const lrcapiDefaultBase = "https://api.lrc.cx"

// 冷查询（首次问酷狗/Apple）实测 8~12 秒，超时给够；命中后它自己会缓存，二次查询 1ms 级。
var lrcapiClient = &http.Client{Timeout: 18 * time.Second}

// lrcapiItem 是 /jsonapi 列表里的一条。
// 注意 score / duration 用 float64：不同版本有时给整数，float64 都能吃。
type lrcapiItem struct {
	Title    string   `json:"title"`
	Artist   string   `json:"artist"`
	Artists  []string `json:"artists"`
	Album    string   `json:"album"`
	LRC      string   `json:"lrc"`     // 新版（1.6+ / 公共实例）
	Lyrics   string   `json:"lyrics"`  // 老版（1.5.x）同一份内容，字段名不同
	TTML     string   `json:"lrc_ttml"`
	Score    float64  `json:"score"`
	Duration float64  `json:"duration"`
}

// lyricText 取歌词正文，兼容 lrc / lyrics 两种字段名。
func (it lrcapiItem) lyricText() string {
	if s := strings.TrimSpace(it.LRC); s != "" {
		return s
	}
	return strings.TrimSpace(it.Lyrics)
}

// ---------- 配置 ----------

func lrcapiEnabled() bool {
	if strings.TrimSpace(os.Getenv("LRCAPI_ENABLED")) == "0" {
		return false
	}
	return lrcapiBase() != ""
}

func lrcapiBase() string {
	base := strings.TrimSpace(os.Getenv("LRCAPI_BASE"))
	if base == "" {
		base = storedLrcapiBase()
	}
	if base == "" {
		base = lrcapiDefaultBase
	}
	base = strings.TrimRight(base, "/")
	// 只认 http/https，避免页面里被填进 file:// 之类的东西
	if u, err := url.Parse(base); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return base
	}
	return lrcapiDefaultBase
}

func lrcapiAuth() string {
	if v := strings.TrimSpace(os.Getenv("LRCAPI_AUTH")); v != "" {
		return v
	}
	return storedLrcapiAuth()
}

// normalizeLrcapiBase 校验并规范化用户填的地址。
// 允许「lrcapi:28883」这种省略协议的写法（自建容器最常见），补成 http://。
func normalizeLrcapiBase(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil // 空 = 恢复默认
	}
	if len(s) > 200 {
		return "", fmt.Errorf("地址太长")
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return "", fmt.Errorf("地址里有空格或不可见字符")
		}
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("地址格式不对")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("只支持 http / https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("地址缺少主机名")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// ownerUserID 返回最早注册的用户。没有角色字段，只能按创建时间认站长。
func (us *UserStore) ownerUserID() string {
	us.mu.RLock()
	defer us.mu.RUnlock()
	best := ""
	var bestAt time.Time
	for id, u := range us.Users {
		if u == nil {
			continue
		}
		if best == "" || u.CreatedAt.Before(bestAt) {
			best, bestAt = id, u.CreatedAt
		}
	}
	return best
}

// settingsOwnerOK 谁能改「服务端要主动去请求的地址」。
// 页面里改 base 等于让服务器替你去访问任意主机，所以只给站长开；
// 其他人仍能看状态、点测试（测试用的是站长配置好的地址）。
func settingsOwnerOK(r *http.Request) bool {
	if pageKeyOK(r) {
		return true
	}
	uid := resolveUserID(r)
	if uid == "" {
		return false
	}
	owner := userStore.ownerUserID()
	return owner != "" && uid == owner
}

// ---------- 取歌词 ----------

func lyricFromLrcAPI(song, singer string) (*lyricResult, error) {
	if !lrcapiEnabled() {
		return nil, fmt.Errorf("lrcapi 未启用")
	}

	base := lrcapiBase()
	q := url.Values{}
	q.Set("title", song)
	if strings.TrimSpace(singer) != "" {
		q.Set("artist", strings.TrimSpace(singer))
	}
	q.Set("limit", "6")
	target := base + "/jsonapi?" + q.Encode()

	started := time.Now()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("lrcapi 请求构造失败")
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0 (lrcapi)")
	req.Header.Set("Accept", "application/json")
	if auth := lrcapiAuth(); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := lrcapiClient.Do(req)
	if err != nil {
		fmt.Printf("[Lyric] lrcapi 请求失败：%v\n", err)
		return nil, fmt.Errorf("lrcapi 连接失败")
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("lrcapi 鉴权失败（%d），检查 LRCAPI_AUTH", resp.StatusCode)
	default:
		return nil, fmt.Errorf("lrcapi 返回 %d", resp.StatusCode)
	}

	body, err := readAllLimited(resp, 2<<20)
	if err != nil {
		return nil, fmt.Errorf("lrcapi 响应读取失败")
	}

	items := parseLrcapiItems(body)
	if len(items) == 0 {
		return nil, fmt.Errorf("lrcapi 无结果")
	}

	best, sc := pickLrcapiItem(items, song, singer)
	if best == nil || sc < 60 {
		return nil, fmt.Errorf("lrcapi 无匹配结果")
	}

	lrc := best.lyricText()
	if lrc == "" && strings.TrimSpace(best.TTML) != "" {
		// Apple Music 的部分曲目只给逐字 TTML，没有 lrc 字段，这里降级转成 LRC
		lrc = ttmlToLRC(best.TTML)
	}
	if lrc == "" {
		return nil, fmt.Errorf("lrcapi 命中但没有歌词内容")
	}
	// 见过的最坏情况：搜一首根本不存在的歌，它把「纯音乐，请欣赏」+ 一堆
	// [ti:xxx] 元信息当成歌词返回。少于 3 行有效内容一律当没找到。
	if n := countLyricContent(lrc); n < 3 {
		return nil, fmt.Errorf("lrcapi 没有有效歌词（%d 行）", n)
	}

	res := &lyricResult{Source: "lrcapi"}
	if countLRCTimestamps(lrc) >= 2 {
		res.LRC = lrc
		res.Synced = true
	} else {
		res.Plain = lrc
	}

	fmt.Printf("[Lyric] lrcapi 命中《%s》/ %s（%d ms，候选 %d 条）\n",
		best.Title, best.Artist, time.Since(started).Milliseconds(), len(items))
	return res, nil
}

// parseLrcapiItems 兼容「单个对象」和「数组」两种返回。
func parseLrcapiItems(body []byte) []lrcapiItem {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []lrcapiItem
		if err := json.Unmarshal(body, &list); err == nil {
			return list
		}
		return nil
	}
	var one lrcapiItem
	if err := json.Unmarshal(body, &one); err == nil && (one.Title != "" || one.lyricText() != "") {
		return []lrcapiItem{one}
	}
	return nil
}

// pickLrcapiItem 按「歌名 + 歌手」复核，挑最像的一条。
func pickLrcapiItem(items []lrcapiItem, song, singer string) (*lrcapiItem, int) {
	if len(items) == 0 {
		return nil, -100
	}
	sorted := make([]lrcapiItem, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		return scoreLrcapiItem(sorted[i], song, singer) > scoreLrcapiItem(sorted[j], song, singer)
	})
	best := sorted[0]
	return &best, scoreLrcapiItem(best, song, singer)
}

func scoreLrcapiItem(it lrcapiItem, song, singer string) int {
	wantTitle := normalizeMatchText(song)
	gotTitle := normalizeMatchText(it.Title)
	wantSinger := normalizeMatchText(singer)
	gotSinger := normalizeMatchText(it.Artist)
	if gotSinger == "" && len(it.Artists) > 0 {
		gotSinger = normalizeMatchText(strings.Join(it.Artists, ""))
	}

	score := 0
	switch {
	case wantTitle == "":
		score -= 60
	case gotTitle == wantTitle:
		score += 70
	case strings.Contains(gotTitle, wantTitle) || strings.Contains(wantTitle, gotTitle):
		score += 35
	default:
		score -= 60
	}

	if wantSinger != "" {
		switch {
		case gotSinger == "":
			score -= 10
		case gotSinger == wantSinger:
			score += 50
		case strings.Contains(gotSinger, wantSinger) || strings.Contains(wantSinger, gotSinger):
			score += 30
		default:
			score -= 45
		}
	}

	// 有时间轴 / 有内容才算「能用的结果」
	if it.lyricText() != "" {
		score += 5
	}
	if it.Score > 0 {
		// 对方自己的相关度只在同分时做微调，不参与主要判断
		score += int(math.Min(it.Score/50, 3))
	}
	return score
}

var bracketRe = regexp.MustCompile(`[\(（\[【].*?[\)）\]】]`)

// normalizeMatchText 把歌名/歌手压成「只留字母数字」的形式做比较：
// 忽略空格、标点、括号里的版本声明（Live / Remaster / 电视剧版…）。
func normalizeMatchText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = bracketRe.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func countLRCTimestamps(lrc string) int {
	return len(lrcTagRe.FindAllStringIndex(lrc, -1))
}

// ---------- TTML -> LRC ----------

var (
	ttmlLineRe  = regexp.MustCompile(`(?s)<p\b([^>]*)>(.*?)</p>`)
	ttmlBeginRe = regexp.MustCompile(`\bbegin="([^"]+)"`)
	ttmlAnyTag  = regexp.MustCompile(`<[^>]*>`)
)

// ttmlToLRC 把 Apple Music 的逐字 TTML 压成按行的时间轴歌词。
// 逐字时间对滚动歌词没用，取每段的起始时间就够了。
func ttmlToLRC(ttml string) string {
	if strings.TrimSpace(ttml) == "" {
		return ""
	}
	var b strings.Builder
	for _, m := range ttmlLineRe.FindAllStringSubmatch(ttml, -1) {
		attrs, inner := m[1], m[2]
		bm := ttmlBeginRe.FindStringSubmatch(attrs)
		if bm == nil {
			continue
		}
		sec, ok := parseTTMLTime(bm[1])
		if !ok {
			continue
		}
		text := html.UnescapeString(ttmlAnyTag.ReplaceAllString(inner, ""))
		text = strings.Join(strings.Fields(text), " ")
		if text == "" || creditLineRe.MatchString(text) {
			continue
		}
		fmt.Fprintf(&b, "[%02d:%05.2f]%s\n", int(sec)/60, math.Mod(sec, 60), text)
	}
	return b.String()
}

// parseTTMLTime 支持 "29.188" 和 "00:00:29.188" 两种写法。
func parseTTMLTime(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if !strings.Contains(v, ":") {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return 0, false
		}
		return f, true
	}
	parts := strings.Split(v, ":")
	if len(parts) > 3 {
		return 0, false
	}
	total := 0.0
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f < 0 {
			return 0, false
		}
		total = total*60 + f
	}
	return total, true
}
