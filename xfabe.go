package main

// 小枫公益 API（xfabe）网易云解析通道
//
// 文档：https://api.xfabe.com/info/wy_MuiscInfo/doc
//
//	搜索  GET https://node.api.xfabe.com/api/wangyi/search?search=<歌名>&limit=15
//	取链  GET https://node.api.xfabe.com/api/wangyi/music?type=json&id=<网易云id>
//
// 与本项目已有的「网易云直连」（netease_direct.go）走的是同一个曲库，差别在
// 取链方式：我们自己拼 music.163.com 的 outer url，只对「免 VIP」的曲目有效；
// 这个接口由第三方后端代取，文档标注「支持 VIP 音乐」，返回的是带鉴权参数的
// CDN 直链（m4a/mp3），理论上能覆盖网易云上那些 fee==1 的付费曲。
//
// 边界与坑（实测）：
//   - /music 接口有服务端全局限流，超出配额时统一返回
//     {"code":405,"msg":"操作频繁，请稍候再试"}，且不分 IP、不分 UA、不分是否
//     首次请求——本机（广州移动）和境外服务器（马来西亚）第一发就是 405。
//     /search 与 /randomMusic 不受影响。所以本源必须做到「一撞 405 立刻闭嘴」，
//     否则每次点歌都要白等一次，链路整体变慢。
//   - 返回的 duration 单位是**毫秒**，别当成秒。
//   - 字段名是 artistsname（少一个 t），照直写。
//
// 因此定位：排在「网易云直连」之后，作为 VIP 曲目的补救通道；不可用时
// 15 分钟内直接跳过，对主链路零开销。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const xfBase = "https://node.api.xfabe.com/api/wangyi"

const xfUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// xfMuteUntil 取链接口被限流后的静默截止时间（unix 秒）。0 表示可用。
var xfMuteUntil atomic.Int64

var xfClient = &http.Client{Timeout: 9 * time.Second}

type xfSong struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Artist   string `json:"artistsname"`
	Album    string `json:"album"`
	Duration int    `json:"duration"` // 毫秒
}

type xfCand struct {
	xfSong
	score int
	fatal bool
}

func xfGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", xfBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", xfUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://api.xfabe.com/")

	resp, err := xfClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 32<<10)
	tmp := make([]byte, 8<<10)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 1<<20 {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// xfSearch 只做「搜」，不取链。limit 建议 15。
func xfSearch(song string, limit int) []xfSong {
	if strings.TrimSpace(song) == "" {
		return nil
	}
	q := "/search?search=" + url.QueryEscape(song) + "&limit=" + strconv.Itoa(limit)
	body, err := xfGet(q)
	if err != nil {
		fmt.Printf("[Xfabe] search error: %v\n", err)
		return nil
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Count int      `json:"count"`
			Songs []xfSong `json:"songs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[Xfabe] search json error: %v\n", err)
		return nil
	}
	if resp.Code != 200 {
		fmt.Printf("[Xfabe] search code=%d msg=%s\n", resp.Code, resp.Msg)
		return nil
	}
	return resp.Data.Songs
}

// xfResolve 按 id 换直链。返回 (url, 毫秒时长)。
// 撞上限流时置静默，避免后续请求继续白等。
func xfResolve(id int64) (string, int) {
	q := "/music?type=json&id=" + strconv.FormatInt(id, 10)
	body, err := xfGet(q)
	if err != nil {
		fmt.Printf("[Xfabe] resolve error: %v\n", err)
		return "", 0
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Artist   string `json:"artistsname"`
			Album    string `json:"album"`
			URL      string `json:"url"`
			Duration int    `json:"duration"`
			Pay      string `json:"pay"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[Xfabe] resolve json error: %v\n", err)
		return "", 0
	}
	if resp.Code == 405 {
		xfMuteUntil.Store(time.Now().Add(15 * time.Minute).Unix())
		fmt.Println("[Xfabe] 取链接口限流(405)，静默 15 分钟")
		return "", 0
	}
	if resp.Code != 200 || !strings.HasPrefix(resp.Data.URL, "http") {
		fmt.Printf("[Xfabe] resolve code=%d msg=%s\n", resp.Code, resp.Msg)
		return "", 0
	}
	return resp.Data.URL, resp.Data.Duration
}

func xfMuted() bool {
	u := xfMuteUntil.Load()
	return u > 0 && time.Now().Unix() < u
}

// XfabeSearch 走 小枫 API 搜歌 + 取链。
//
// 打分沿用项目里那套公共闸门（版本噪声 / 标题长度 / 歌手翻唱标记），
// 只对分数最高的前 3 条去取链，避免一次点歌打三次接口。
func XfabeSearch(song, singer string) MusicItem {
	if strings.TrimSpace(song) == "" || xfMuted() {
		return MusicItem{}
	}

	list := xfSearch(song, 15)
	if len(list) == 0 {
		fmt.Printf("[Xfabe] no result for %q - %q\n", song, singer)
		return MusicItem{}
	}

	wantName := kwNormName(song)
	wantArtist := kwNormName(singer)
	var cands []xfCand

	for _, t := range list {
		score := 0
		fatal := false

		ts, tfatal := srcTitleScore(kwNormName(t.Name), wantName)
		score += ts
		if tfatal {
			fatal = true
		}
		if pen, fat, _ := srcNoiseOf(t.Name, t.Album, song); fat {
			score += pen
			fatal = true
		}

		// 歌手：网易云的 artistsname 可能是「A/B」多歌手串
		gotArtist := kwNormName(t.Artist)
		switch {
		case wantArtist == "":
			score += 20
		case gotArtist == wantArtist:
			score += 90
		case strings.Contains(gotArtist, wantArtist) || strings.Contains(wantArtist, gotArtist):
			score += 55
		default:
			score -= 35
		}
		lowerArtist := strings.ToLower(t.Artist)
		for _, m := range kwFatalArtistMarkers {
			if strings.Contains(lowerArtist, m) {
				score -= 200
				fatal = true
				break
			}
		}

		// 时长闸门：少于 60 秒的多半是试听/切片
		if t.Duration > 0 && t.Duration < 60000 {
			score -= 80
			fatal = true
		}

		cands = append(cands, xfCand{xfSong: t, score: score, fatal: fatal})
	}

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	tried := 0
	for i, c := range cands {
		if c.fatal || c.score < 80 {
			continue
		}
		if tried >= 3 {
			break
		}
		tried++
		if i < 3 {
			fmt.Printf("[Xfabe] cand #%d score=%d \"%s\" - \"%s\" (%s, %ds)\n",
				i+1, c.score, c.Name, c.Artist, c.Album, c.Duration/1000)
		}

		audioURL, durMs := xfResolve(c.ID)
		if audioURL == "" {
			if xfMuted() {
				return MusicItem{}
			}
			continue
		}
		if durMs == 0 {
			durMs = c.Duration
		}
		fmt.Printf("[Xfabe] HIT #%d \"%s\" - \"%s\" (%ds, score=%d)\n",
			i+1, c.Name, c.Artist, durMs/1000, c.score)
		return MusicItem{
			Title:    c.Name,
			Artist:   c.Artist,
			AudioURL: audioURL,
			Duration: durMs / 1000,
		}
	}
	return MusicItem{}
}
