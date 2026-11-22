package main

// 喜马拉雅（有声书 / 播客 / 广播剧 / 翻唱）
//
// 定位：**只在「喜马拉雅」这个筛选按钮下出现**的补充源。
// 它的曲库和音乐源完全不重叠，混进「全部」会把搜索结果冲成一堆播客，
// 所以默认不参与 getRemoteMusicURL 的多源链路，也不参与「全部」搜索。
//
// 走网页版公开接口（2026-11-23 在东京机器实测可直连）：
//
//	搜索  GET /revision/search?core=track&kw=<关键词>&device=web
//	      → data.result.response.docs[]，字段：id / title / nickname / duration(秒) / is_paid
//	取链  GET /revision/play/tracks?trackIds=<trackId>
//	      → data.tracksForAudioPlay[0].src（m4a），**只有免费内容才有 src**
//
// 网页接口前面有 WAF，必须先拿到 HWWAFSESID 这类 cookie，否则直接 400。
// 这里用带 cookiejar 的 client，第一次请求前预热首页，失败/过期自动重来一次。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sourceXimalaya = "ximalaya"
	ximalayaBase   = "https://www.ximalaya.com"
	ximalayaUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// cookie 半小时重打一次，够用又不至于每次请求都多跑一个往返
	ximalayaCookieTTL = 30 * time.Minute
)

// xmTrack 是喜马拉雅的一集/一条音频
type xmTrack struct {
	ID       string
	Name     string
	Artist   string // 主播/上传者
	Album    string
	Duration int // 秒
	Cover    string
	Paid     bool
}

var (
	xmClientOnce sync.Once
	xmClient     *http.Client

	xmCookieMu sync.Mutex
	xmCookieAt time.Time
)

// ximalayaEnabled 开关：XIMALAYA_ENABLED=0 关掉（默认开）
func ximalayaEnabled() bool {
	v := strings.TrimSpace(os.Getenv("XIMALAYA_ENABLED"))
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "off")
}

func ximalayaClient() *http.Client {
	xmClientOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		xmClient = &http.Client{
			Timeout: 20 * time.Second,
			Jar:     jar,
		}
	})
	return xmClient
}

// ximalayaWarmup 预热网页 cookie（HWWAFSESID 等）。force=false 时按 TTL 复用。
func ximalayaWarmup(force bool) {
	xmCookieMu.Lock()
	defer xmCookieMu.Unlock()
	if !force && !xmCookieAt.IsZero() && time.Since(xmCookieAt) < ximalayaCookieTTL {
		return
	}
	req, err := http.NewRequest(http.MethodGet, ximalayaBase+"/", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", ximalayaUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := ximalayaClient().Do(req)
	if err != nil {
		fmt.Printf("[ximalaya] cookie 预热门失败: %v\n", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	xmCookieAt = time.Now()
}

// ximalayaGet 带 cookie + 反代的 GET，返回 body。400/403 时重打 cookie 再试一次。
func ximalayaGet(rawURL, referer string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ximalayaWarmup(attempt > 0)

		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ximalayaUA)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := ximalayaClient().Do(req)
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
	}
	return nil, lastErr
}

// xmFlexID 兼容接口里 id 有时是数字、有时是字符串
type xmFlexID string

func (f *xmFlexID) UnmarshalJSON(b []byte) error {
	*f = xmFlexID(strings.Trim(string(b), `"`))
	return nil
}

type xmSearchResp struct {
	Ret  int `json:"ret"`
	Data struct {
		Result struct {
			Response struct {
				Docs []struct {
					ID         xmFlexID `json:"id"`
					Title      string   `json:"title"`
					Nickname   string   `json:"nickname"`
					AlbumTitle string   `json:"album_title"`
					Duration   float64  `json:"duration"` // 秒
					IsPaid     bool     `json:"is_paid"`
					Cover      string   `json:"cover_path"`
				} `json:"docs"`
			} `json:"response"`
		} `json:"result"`
	} `json:"data"`
}

type xmPlayResp struct {
	Ret  int `json:"ret"`
	Data struct {
		Tracks []struct {
			TrackID   xmFlexID `json:"trackId"`
			TrackName string   `json:"trackName"`
			Src       string   `json:"src"`
			Duration  float64  `json:"duration"`
			IsPaid    bool     `json:"isPaid"`
			Cover     string   `json:"trackCoverPath"`
			AlbumName string   `json:"albumName"`
		} `json:"tracksForAudioPlay"`
	} `json:"data"`
}

// xmCoverURL 接口给的是 //imagev2.xmcdn.com/...，补成 https
func xmCoverURL(p string) string {
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

func ximalayaSearchRaw(keyword string, limit int) []xmTrack {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || limit <= 0 {
		return nil
	}
	if limit > 50 {
		limit = 50
	}
	rawURL := fmt.Sprintf("%s/revision/search?core=track&kw=%s&page=1&rows=%d"+
		"&spellchecker=true&condition=relation&device=web",
		ximalayaBase, url.QueryEscape(keyword), limit)

	body, err := ximalayaGet(rawURL, ximalayaBase+"/search/"+url.QueryEscape(keyword))
	if err != nil {
		fmt.Printf("[ximalaya] 搜索失败 %q: %v\n", keyword, err)
		return nil
	}
	var resp xmSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[ximalaya] 搜索响应解析失败 %q: %v\n", keyword, err)
		return nil
	}

	out := make([]xmTrack, 0, len(resp.Data.Result.Response.Docs))
	for _, d := range resp.Data.Result.Response.Docs {
		name := strings.TrimSpace(d.Title)
		if name == "" || d.ID == "" {
			continue
		}
		dur := int(d.Duration)
		out = append(out, xmTrack{
			ID:       string(d.ID),
			Name:     name,
			Artist:   strings.TrimSpace(d.Nickname),
			Album:    strings.TrimSpace(d.AlbumTitle),
			Duration: dur,
			Cover:    xmCoverURL(d.Cover),
			Paid:     d.IsPaid,
		})
	}
	return out
}

// XimalayaSearchLimit 供「喜马拉雅」单源搜索用：免费内容排前面（付费的没有播放地址）。
func XimalayaSearchLimit(song, singer string, limit int) []xmTrack {
	if !ximalayaEnabled() {
		return nil
	}
	kw := strings.TrimSpace(song + " " + singer)
	list := ximalayaSearchRaw(kw, limit)
	// 歌名+歌手拼起来可能一个都搜不到（喜马拉雅的检索偏标题），退回只搜歌名
	if len(list) == 0 && singer != "" && strings.TrimSpace(song) != "" {
		list = ximalayaSearchRaw(song, limit)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Paid != list[j].Paid {
			return !list[i].Paid
		}
		return false
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

// XimalayaTrackSrc 按 trackId 取播放地址（m4a）。
// 付费内容没有 src，返回一个能直接提示给用户的错误。
func XimalayaTrackSrc(trackID string) (string, error) {
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return "", fmt.Errorf("缺少 trackId")
	}
	if _, err := strconv.ParseInt(trackID, 10, 64); err != nil {
		return "", fmt.Errorf("trackId 格式不对: %s", trackID)
	}

	rawURL := ximalayaBase + "/revision/play/tracks?trackIds=" + url.QueryEscape(trackID)
	body, err := ximalayaGet(rawURL, ximalayaBase+"/sound/"+trackID)
	if err != nil {
		return "", err
	}
	var resp xmPlayResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("播放信息解析失败: %w", err)
	}
	if len(resp.Data.Tracks) == 0 {
		return "", fmt.Errorf("该音频不存在或已下架")
	}
	t := resp.Data.Tracks[0]
	if strings.TrimSpace(t.Src) == "" {
		if t.IsPaid {
			return "", fmt.Errorf("该音频需要喜马拉雅会员，本站无法播放")
		}
		return "", fmt.Errorf("该音频暂无可播放地址")
	}
	// 统一走 https，省得以后站点上 https 时出现混合内容
	src := strings.TrimSpace(t.Src)
	if strings.HasPrefix(src, "http://") {
		src = "https://" + strings.TrimPrefix(src, "http://")
	}
	return src, nil
}
