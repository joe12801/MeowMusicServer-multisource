package main

// B 站（Bilibili）音源
//
// 定位：和喜马拉雅一样，是**只在「B站」这个筛选按钮下出现**的补充源。
// 曲库里是 UP 主上传的现场、修复版、Hi-Res 二创、冷门 Live——国内四大家
// 覆盖不到的那部分。混进「全部」会把这些视频结果排到搜歌结果前面，
// 所以默认不参与 getRemoteMusicURL 的多源链路，也不参与「全部」搜索。
//
// 走公开 Web 接口（2026-11-23 在境外机器实测可直连）：
//
//	搜索  GET api.bilibili.com/x/web-interface/search/type?search_type=video&keyword=..
//	      → data.result[]，字段：bvid / title(带 <em> 高亮) / author / pic / duration("4:30") / play
//	详情  GET api.bilibili.com/x/web-interface/view?bvid=..
//	      → data.pages[0].cid（取流必须带 cid）、title / pic / owner.name / duration
//	取流  GET api.bilibili.com/x/player/playurl?fnval=4048&bvid=..&cid=..
//	      → data.dash.{flac,dolby,audio}[].baseUrl（DASH 纯音频，按 bandwidth 取最高）
//
// 三个坑（都实测过）：
//  1. 搜索接口不带 Cookie 会 412，随便塞一个 buvid3 就能过；
//  2. 音频 CDN 校验 User-Agent，ffmpeg 默认的 "Lavf/x.y" 直接 403，
//     必须 -user_agent 传浏览器 UA（见 music_cache.go 的 ensureMusicCacheWithHeaders）；
//  3. 没有 flac/dolby 时 audio[] 通常只有 64k/132k/192k 三档，取最高即可，
//     反正本站统一转 32k 单声道。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sourceBilibili = "bilibili"
	biliAPIBase    = "https://api.bilibili.com"
	biliReferer    = "https://www.bilibili.com/"
	biliUA         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	biliSearchPageSize = 50
)

// biliTrack 是一条 B 站视频（只取和播放有关的信息）
type biliTrack struct {
	BVID     string
	Title    string
	Artist   string // UP 主
	Duration int    // 秒
	Cover    string
	Play     int64
}

// biliAudio 是一次取流的结果：主地址 + 备用 CDN 地址
type biliAudio struct {
	URL       string
	Backups   []string
	Title     string
	Artist    string
	Cover     string
	Duration  int
	Bandwidth int64
	Lossless  bool
}

var (
	biliClientOnce sync.Once
	biliClient     *http.Client

	biliCookieOnce sync.Once
	biliCookie     string

	biliBVIDRe  = regexp.MustCompile(`^BV[0-9A-Za-z]{10}$`)
	biliTagRe   = regexp.MustCompile(`<[^>]*>`)
	biliBracket = regexp.MustCompile(`【[^】]*】`)
	biliSpaceRe = regexp.MustCompile(`\s+`)
)

// bilibiliEnabled 开关：BILI_ENABLED=0 关掉（默认开）
func bilibiliEnabled() bool {
	v := strings.TrimSpace(os.Getenv("BILI_ENABLED"))
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "off")
}

func biliHTTPClient() *http.Client {
	biliClientOnce.Do(func() {
		biliClient = &http.Client{Timeout: 20 * time.Second}
	})
	return biliClient
}

// biliCookieHeader 返回访问 B 站用的 Cookie。
// 优先用 BILI_COOKIE 环境变量（用户想带 SESSDATA 拿更高码率/大会员内容时），
// 否则本地随机生成一个 buvid3——匿名请求有这个就够，不需要真的去指纹接口取。
func biliCookieHeader() string {
	biliCookieOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("BILI_COOKIE")); v != "" {
			biliCookie = v
			return
		}
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			biliCookie = fmt.Sprintf("buvid3=%dinfoc", time.Now().UnixNano())
			return
		}
		biliCookie = "buvid3=" + strings.ToUpper(hex.EncodeToString(buf)) + "infoc"
	})
	return biliCookie
}

// biliStreamHeaders 是 ffmpeg 拉音频流时必须带上的头（UA 是硬要求，见文件头注释）。
func biliStreamHeaders() map[string]string {
	return map[string]string{
		"User-Agent": biliUA,
		"Referer":    biliReferer,
		"Cookie":     biliCookieHeader(),
	}
}

// biliGet 带 UA / Referer / Cookie 请求 B 站 API，返回 body。
func biliGet(rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", biliUA)
		req.Header.Set("Referer", biliReferer)
		req.Header.Set("Cookie", biliCookieHeader())
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		resp, err := biliHTTPClient().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}
		lastErr = fmt.Errorf("http %d", resp.StatusCode)
		// 412 是风控（多半 cookie 不被认），换一个重试一次没意义，直接返回
		if resp.StatusCode == http.StatusPreconditionFailed {
			break
		}
	}
	return nil, lastErr
}

type biliSearchResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Result []struct {
			Type     string `json:"type"`
			BVID     string `json:"bvid"`
			Title    string `json:"title"`
			Author   string `json:"author"`
			Pic      string `json:"pic"`
			Duration string `json:"duration"` // "4:30" / "1:02:03"
			Play     int64  `json:"play"`
		} `json:"result"`
	} `json:"data"`
}

type biliViewResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		BVID     string `json:"bvid"`
		Title    string `json:"title"`
		Pic      string `json:"pic"`
		Duration int    `json:"duration"`
		Owner    struct {
			Name string `json:"name"`
		} `json:"owner"`
		Pages []struct {
			CID      int64  `json:"cid"`
			Part     string `json:"part"`
			Duration int    `json:"duration"`
		} `json:"pages"`
	} `json:"data"`
}

type biliAudioStream struct {
	ID        int      `json:"id"`
	BaseURL   string   `json:"baseUrl"`
	BaseURLSn string   `json:"base_url"`
	Backup    []string `json:"backupUrl"`
	BackupSn  []string `json:"backup_url"`
	Bandwidth int64    `json:"bandwidth"`
	CodecID   int      `json:"codecid"`
}

func (s biliAudioStream) primary() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return s.BaseURLSn
}

func (s biliAudioStream) backups() []string {
	if len(s.Backup) > 0 {
		return s.Backup
	}
	return s.BackupSn
}

type biliPlayResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Dash *struct {
			Duration float64           `json:"duration"`
			Audio    []biliAudioStream `json:"audio"`
			Flac     *struct {
				Audio []biliAudioStream `json:"audio"`
			} `json:"flac"`
			Dolby *struct {
				Audio []biliAudioStream `json:"audio"`
			} `json:"dolby"`
		} `json:"dash"`
		// 极少数老视频没有 DASH，只有整段 mp4，用它兜底（ffmpeg 抽音轨）
		Durl []struct {
			URL string `json:"url"`
		} `json:"durl"`
	} `json:"data"`
}

// biliCoverURL 接口给的是 //i0.hdslb.com/...，补成 https
func biliCoverURL(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "//") {
		return "https:" + p
	}
	if strings.HasPrefix(p, "http://") {
		return "https://" + strings.TrimPrefix(p, "http://")
	}
	return p
}

// biliCleanText 去掉搜索高亮 <em> 标签并反转义 HTML 实体
func biliCleanText(s string) string {
	s = biliTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(biliSpaceRe.ReplaceAllString(s, " "))
}

// biliParseDuration 把 "4:30" / "1:02:03" 转成秒
func biliParseDuration(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	total, ok := 0, false
	for _, part := range strings.Split(s, ":") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return 0
		}
		total = total*60 + n
		ok = true
	}
	if !ok {
		return 0
	}
	return total
}

// biliSearchRaw 直接调搜索接口
func biliSearchRaw(keyword string, limit int) []biliTrack {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || limit <= 0 {
		return nil
	}
	pageSize := limit
	if pageSize > biliSearchPageSize {
		pageSize = biliSearchPageSize
	}
	q := url.Values{}
	q.Set("search_type", "video")
	q.Set("keyword", keyword)
	q.Set("page", "1")
	q.Set("page_size", strconv.Itoa(pageSize))

	body, err := biliGet(biliAPIBase + "/x/web-interface/search/type?" + q.Encode())
	if err != nil {
		fmt.Printf("[bilibili] 搜索失败 %q: %v\n", keyword, err)
		return nil
	}
	var resp biliSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[bilibili] 搜索响应解析失败 %q: %v\n", keyword, err)
		return nil
	}
	if resp.Code != 0 {
		fmt.Printf("[bilibili] 搜索返回 code=%d msg=%s（关键词 %q）\n", resp.Code, resp.Message, keyword)
		return nil
	}

	out := make([]biliTrack, 0, len(resp.Data.Result))
	for _, v := range resp.Data.Result {
		bvid := strings.TrimSpace(v.BVID)
		if bvid == "" || !biliBVIDRe.MatchString(bvid) {
			continue
		}
		// search_type=video 偶尔会混进合辑/番剧，只留普通视频
		if v.Type != "" && v.Type != "video" {
			continue
		}
		title := biliCleanText(v.Title)
		if title == "" {
			continue
		}
		out = append(out, biliTrack{
			BVID:     bvid,
			Title:    title,
			Artist:   biliCleanText(v.Author),
			Duration: biliParseDuration(v.Duration),
			Cover:    biliCoverURL(v.Pic),
			Play:     v.Play,
		})
	}
	return out
}

// BilibiliSearchLimit 供「B站」单源搜索用。
// 只走这一条筛选路径：视频标题噪声大，混进「全部」会把正常搜歌结果挤下去。
func BilibiliSearchLimit(song, singer string, limit int) []biliTrack {
	if !bilibiliEnabled() {
		return nil
	}
	song, singer = strings.TrimSpace(song), strings.TrimSpace(singer)
	if song == "" && singer == "" {
		return nil
	}
	if limit <= 0 {
		return nil
	}
	kw := strings.TrimSpace(song + " " + singer)
	list := biliSearchRaw(kw, limit)
	// 歌名+UP主 拼一起很可能一条都搜不到，退回只搜歌名
	if len(list) == 0 && singer != "" && song != "" {
		list = biliSearchRaw(song, limit)
	}
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

// BilibiliAudio 按 BV 号取纯音频直链。
// 只取视频的第一 P（多 P 合集里通常是完整曲目）。
func BilibiliAudio(bvid string) (*biliAudio, error) {
	bvid = strings.TrimSpace(bvid)
	if !biliBVIDRe.MatchString(bvid) {
		return nil, fmt.Errorf("BV 号格式不对: %s", bvid)
	}

	body, err := biliGet(biliAPIBase + "/x/web-interface/view?bvid=" + url.QueryEscape(bvid))
	if err != nil {
		return nil, fmt.Errorf("取视频信息失败: %w", err)
	}
	var view biliViewResp
	if err := json.Unmarshal(body, &view); err != nil {
		return nil, fmt.Errorf("视频信息解析失败: %w", err)
	}
	if view.Code != 0 {
		return nil, biliCodeError("取视频信息", view.Code, view.Message)
	}
	if len(view.Data.Pages) == 0 {
		return nil, fmt.Errorf("该视频不存在或已下架")
	}
	cid := view.Data.Pages[0].CID
	if cid <= 0 {
		return nil, fmt.Errorf("该视频没有可取流的 cid")
	}

	out := &biliAudio{
		Title:    biliCleanText(view.Data.Title),
		Artist:   biliCleanText(view.Data.Owner.Name),
		Cover:    biliCoverURL(view.Data.Pic),
		Duration: view.Data.Duration,
	}

	q := url.Values{}
	q.Set("fnval", "4048") // 16=DASH + 64/128/256/512/1024/2048，一次要全
	q.Set("fourk", "1")
	q.Set("bvid", bvid)
	q.Set("cid", strconv.FormatInt(cid, 10))

	body, err = biliGet(biliAPIBase + "/x/player/playurl?" + q.Encode())
	if err != nil {
		return nil, fmt.Errorf("取播放地址失败: %w", err)
	}
	var play biliPlayResp
	if err := json.Unmarshal(body, &play); err != nil {
		return nil, fmt.Errorf("播放地址解析失败: %w", err)
	}
	if play.Code != 0 {
		return nil, biliCodeError("取播放地址", play.Code, play.Message)
	}

	// DASH 纯音频：flac（无损）> dolby > 普通 AAC，按 bandwidth 取最高即可
	var streams []biliAudioStream
	if d := play.Data.Dash; d != nil {
		if d.Flac != nil {
			streams = append(streams, d.Flac.Audio...)
		}
		if d.Dolby != nil {
			streams = append(streams, d.Dolby.Audio...)
		}
		streams = append(streams, d.Audio...)
	}
	sort.SliceStable(streams, func(i, j int) bool {
		return streams[i].Bandwidth > streams[j].Bandwidth
	})

	for _, s := range streams {
		u := strings.TrimSpace(s.primary())
		if !strings.HasPrefix(u, "http") {
			continue
		}
		out.URL = biliNormalizeURL(u)
		out.Bandwidth = s.Bandwidth
		out.Lossless = s.CodecID == 0 && s.Bandwidth >= 500000 // flac 基本都在 500k 以上
		for _, b := range s.backups() {
			b = biliNormalizeURL(strings.TrimSpace(b))
			if b != "" && b != out.URL {
				out.Backups = append(out.Backups, b)
			}
		}
		return out, nil
	}

	// 兜底：老的 durl 整段 mp4，ffmpeg 能抽出音轨
	for _, d := range play.Data.Durl {
		if u := strings.TrimSpace(d.URL); strings.HasPrefix(u, "http") {
			out.URL = biliNormalizeURL(u)
			return out, nil
		}
	}
	return nil, fmt.Errorf("该视频没有可用的音频流（可能是付费/限定内容）")
}

// biliSongKeyword 从视频标题里抽核心歌名，用于失效视频的兜底搜索。
//
// 搜索结果的标题长这样：
//
//	在百万豪装录音棚大声听陈奕迅《富士山下》【Hi-res】
//	【𝐇𝐢-𝐑𝐞𝐬无损音质】｜《晴天》- 周杰伦 -‘故事的小黄花’
//
// 直接拿整条当关键词搜出来的东西很随机，抽《》里的名字最准；
// 没有书名号就去掉【】标签、砍掉 " - " 之后的花絮部分。
func biliSongKeyword(title string) string {
	t := strings.TrimSpace(title)
	if i := strings.Index(t, "《"); i >= 0 {
		rest := t[i+len("《"):]
		if j := strings.Index(rest, "》"); j > 0 {
			if s := strings.TrimSpace(rest[:j]); s != "" {
				return strings.TrimSpace(biliBracket.ReplaceAllString(s, " "))
			}
		}
	}
	t = strings.TrimSpace(biliBracket.ReplaceAllString(t, " "))
	if i := strings.Index(t, " - "); i > 0 {
		t = strings.TrimSpace(t[:i])
	}
	t = strings.TrimSpace(biliSpaceRe.ReplaceAllString(t, " "))
	// 关键词太长反而搜不到（B 站按相关性排），截到 30 个字
	if r := []rune(t); len(r) > 30 {
		t = string(r[:30])
	}
	return t
}

// biliLooksLikeCover 判断一条结果是不是「伴奏 / 教学 / 翻唱」这类不适合当兜底的版本。
func biliLooksLikeCover(title string) bool {
	t := strings.ToLower(title)
	for _, w := range []string{"伴奏", "karaoke", "卡拉ok", "纯音乐", "钢琴", "吉他", "鼓谱",
		"教学", "教程", "翻唱", "歌单", "合集"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// BilibiliResolve 按 BV 号取流；失败时用同名搜索兜底换一条能播的。
//
// 为什么要兜底：B 站搜索索引里约 6% 的视频已经删除/仅限地区（实测 49 条里 3 条
// 返回 -404），用户点到这种条目就是一次「点了没反应」。这些候选本来就是同一个
// 歌的不同投稿，换一条播比直接报错有用得多。
func BilibiliResolve(bv, song, singer string) (*biliAudio, string, error) {
	audio, err := BilibiliAudio(bv)
	if err == nil {
		return audio, bv, nil
	}
	kw := biliSongKeyword(song)
	if kw == "" {
		return nil, bv, err
	}

	fmt.Printf("[bilibili] %s 取流失败（%v），用关键词 %q 兜底\n", bv, err, kw)
	list := BilibiliSearchLimit(kw, singer, 8)
	// 跑两轮：第一轮跳过伴奏/教程/歌单合集，第二轮实在没有能播的就放宽
	for pass := 0; pass < 2; pass++ {
		for _, t := range list {
			if t.BVID == bv {
				continue
			}
			if pass == 0 && biliLooksLikeCover(t.Title) {
				continue
			}
			if a, e := BilibiliAudio(t.BVID); e == nil && a.URL != "" {
				fmt.Printf("[bilibili] 兜底命中 %s（%s）\n", t.BVID, t.Title)
				return a, t.BVID, nil
			}
		}
	}
	return nil, bv, err
}

func biliNormalizeURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// biliCodeError 把 B 站的业务错误码翻译成用户能看懂的话
func biliCodeError(stage string, code int, msg string) error {
	switch code {
	case -403:
		return fmt.Errorf("%s失败：该视频需要登录或大会员权限", stage)
	case -404:
		return fmt.Errorf("%s失败：视频不存在或已删除", stage)
	case -412:
		return fmt.Errorf("%s失败：请求被 B 站风控拦截，稍后再试", stage)
	case -352:
		return fmt.Errorf("%s失败：B 站风控校验未通过，稍后再试", stage)
	case 87008:
		return fmt.Errorf("%s失败：该视频仅限特定地区观看", stage)
	}
	return fmt.Errorf("%s失败：code=%d %s", stage, code, strings.TrimSpace(msg))
}
