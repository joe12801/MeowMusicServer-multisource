package main

// 网易云「直连」通道：不依赖本地 ncm API 服务，直接打 music.163.com。
//
// 为什么需要它：原有 NCMSearch 要求本机跑一个 node 版网易云 API
// （ncmAPIBase），NAS / 境外服务器上根本没装 node，这条链路等于永久失效。
// 实测 music.163.com 本身对外网开放搜索，且带上首页下发的 cookie 之后
// /song/media/outer/url 会 302 到真实 CDN 并返回音频，无需任何密钥。
//
// 边界（重要）：
//   - 只覆盖「有版权且不需要 VIP」的曲目。fee==1 的 VIP 曲、fee==8 的
//     下架/无版权曲，outer url 会返回 107KB 的 HTML 页面而不是音频，
//     必须在 Content-Type 上识别出来并当失败处理，否则会把 HTML 丢给
//     ffmpeg，设备侧表现为「一直转圈然后静音」。
//   - 周杰伦这类整批下架的歌手本源基本无解，交给酷我。
//
// 因此本源的定位是：酷我 + 酷狗都空手时，兜住那些「网易云有版权、
// 免 VIP」的曲目（独立音乐、民谣、部分华语流行）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var neBlocked atomic.Bool

const neUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

var (
	neClient   *http.Client
	neClientMu sync.Mutex
	neBootOnce sync.Once
)

// neHTTP 惰性初始化带 cookie jar 的客户端。cookie（NMTID 等）是取链的前提，
// 没有它就是 107KB HTML。首页只需要打一次，之后 jar 自己维持。
func neHTTP() *http.Client {
	neClientMu.Lock()
	defer neClientMu.Unlock()
	if neClient != nil {
		return neClient
	}
	jar, _ := cookiejar.New(nil)
	neClient = &http.Client{
		Jar:     jar,
		Timeout: 8 * time.Second,
		// outer url 会 302 到 CDN，跟随后从 resp.Request.URL 取最终地址
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return neClient
}

func neGet(rawURL string, ref bool) (*http.Response, error) {
	c := neHTTP()
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", neUA)
	req.Header.Set("Accept", "*/*")
	if ref {
		req.Header.Set("Referer", "https://music.163.com/")
	}
	return c.Do(req)
}

// neWarmup 取一次首页把 cookie 灌进 jar。失败不致命，后续请求可能仍能用。
func neWarmup() {
	neBootOnce.Do(func() {
		resp, err := neGet("https://music.163.com/", false)
		if err != nil {
			fmt.Printf("[Netease] warmup error: %v\n", err)
			return
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	})
}

type neSong struct {
	ID       int64
	Name     string
	Artist   string
	Album    string
	Duration int // 秒
	Fee      int
	score    int
	fatal    bool
	noVocal  bool
}

type neSearchResp struct {
	Result struct {
		Songs []struct {
			ID       int64 `json:"id"`
			Name     string `json:"name"`
			Fee      int    `json:"fee"`
			Duration int    `json:"duration"` // 毫秒
			Artists  []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name string `json:"name"`
			} `json:"album"`
		} `json:"songs"`
	} `json:"result"`
}

func neSearch(song, singer string) []neSong {
	neWarmup()
	keyword := strings.TrimSpace(song + " " + singer)
	api := "https://music.163.com/api/search/get/web?s=" + url.QueryEscape(keyword) +
		"&type=1&offset=0&limit=15&total=true"

	resp, err := neGet(api, true)
	if err != nil {
		fmt.Printf("[Netease] search error: %v\n", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	var r neSearchResp
	if err := json.Unmarshal(body, &r); err != nil {
		fmt.Printf("[Netease] search json error: %v (%.80s)\n", err, string(body))
		return nil
	}
	out := make([]neSong, 0, len(r.Result.Songs))
	for _, s := range r.Result.Songs {
		names := make([]string, 0, len(s.Artists))
		for _, a := range s.Artists {
			names = append(names, a.Name)
		}
		out = append(out, neSong{
			ID:       s.ID,
			Name:     s.Name,
			Artist:   strings.Join(names, "/"),
			Album:    s.Album.Name,
			Duration: s.Duration / 1000,
			Fee:      s.Fee,
		})
	}
	return out
}

func neScoreTrack(t *neSong, song, singer string) (int, bool) {
	score := 0
	fatal := false

	wantName := kwNormName(song)
	gotName := kwNormName(t.Name)
	ts, tfatal := srcTitleScore(gotName, wantName)
	score += ts
	if tfatal {
		fatal = true
	}

	if wantSinger := strings.ToLower(strings.TrimSpace(singer)); wantSinger != "" && wantSinger != "未知" {
		got := strings.ToLower(t.Artist)
		if kwArtistMatch(got, wantSinger) {
			score += 60
			if got == wantSinger {
				score += 10
			}
		} else if got == "" {
			score -= 20
		} else {
			score -= 70
		}
	}

	if pen, fat, nv := srcNoiseOf(t.Name, t.Album, song); fat {
		score += pen
		fatal = true
		t.noVocal = nv
	}

	if t.Duration > 0 && t.Duration < 60 {
		score -= 80
		fatal = true
	}
	return score, fatal
}

func neRank(list []neSong, song, singer string) []neSong {
	for i := range list {
		list[i].score, list[i].fatal = neScoreTrack(&list[i], song, singer)
	}
	var durs []int
	for _, t := range list {
		if t.Duration >= 60 && t.Duration <= 600 {
			durs = append(durs, t.Duration)
		}
	}
	median := 0
	if len(durs) >= 3 {
		sort.Ints(durs)
		median = durs[len(durs)/2]
		for i := range list {
			if list[i].Duration > 0 && float64(list[i].Duration) < 0.55*float64(median) {
				list[i].score -= 140
				list[i].fatal = true
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		if median > 0 && list[i].Duration > 0 && list[j].Duration > 0 {
			di := absInt(list[i].Duration - median)
			dj := absInt(list[j].Duration - median)
			if di != dj {
				return di < dj
			}
		}
		return list[i].Duration > list[j].Duration
	})
	return list
}

// nePlayURL 取最终音频地址。返回空串表示这首拿不到（无版权 / 需 VIP / 被限流）。
//
// 判定依据是 Content-Type：拿到 audio/* 才算成功；text/html 说明被重定向到
// 了「该歌曲暂无版权」的页面。同时读取开头的字节确保不是 0 字节的空响应。
func nePlayURL(id int64) string {
	outer := fmt.Sprintf("https://music.163.com/song/media/outer/url?id=%d.mp3", id)
	resp, err := neGet(outer, true)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "audio") && !strings.Contains(ct, "octet-stream") &&
		!strings.Contains(ct, "mpeg") {
		// HTML 页面：这首没有可用音源
		io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		return ""
	}
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	if !strings.HasPrefix(final, "http") {
		return ""
	}
	return final
}

// NeteaseSearch 对外入口：直连网易云，不依赖本地 API 服务。
func NeteaseSearch(song, singer string) MusicItem {
	if strings.TrimSpace(song) == "" || neBlocked.Load() {
		return MusicItem{}
	}
	list := neSearch(song, singer)
	if len(list) == 0 {
		fmt.Printf("[Netease] no result for %q - %q\n", song, singer)
		return MusicItem{}
	}
	fmt.Printf("[Netease] search -> %d hits\n", len(list))
	if len(list) > 12 {
		list = list[:12]
	}
	ranked := neRank(list, song, singer)

	var queue []neSong
	for _, t := range ranked {
		// fee==1 是 VIP 专享、fee==4 是付费专辑，outer url 必空，直接跳过省时间
		if !t.fatal && t.score >= 40 && t.Fee != 1 && t.Fee != 4 {
			queue = append(queue, t)
		}
	}
	// 注意：这里**不做**「没有原唱就退而求其次用现场版/改编版」的兜底。
	// 实测放开后点《成都》会放成「(2026没有信号巡回演唱会北京站)」、
	// 点《大鱼》放成「(3D环绕版)」，用户一听就知道错了。宁可这里返回空，
	// 让后面的源去试真正的原唱；所有源都没有时，最后的 GDStudio / yuafeng
	// 那一层保留了宽容策略，兜底不至于断歌。
	if len(queue) > 4 {
		queue = queue[:4]
	}

	for i, t := range queue {
		if i < 2 {
			fmt.Printf("[Netease] cand #%d score=%d fee=%d %q - %q (%ds)\n",
				i+1, t.score, t.Fee, t.Name, t.Artist, t.Duration)
		}
		audioURL := nePlayURL(t.ID)
		if audioURL == "" {
			continue
		}
		fmt.Printf("[Netease] HIT #%d %q - %q (%ds, score=%d)\n", i+1, t.Name, t.Artist, t.Duration, t.score)
		return MusicItem{
			Title:        t.Name,
			Artist:       t.Artist,
			AudioURL:     audioURL,
			AudioFullURL: audioURL,
			Duration:     t.Duration,
		}
	}
	fmt.Printf("[Netease] all candidates unusable for %q - %q\n", song, singer)
	return MusicItem{}
}
