package main

// 酷狗通道：曲库覆盖面最广的国产源之一，作为酷我之后的第二顺位兜底。
//
// 为什么需要它：酷我波点通道能拿付费大牌（周杰伦、陈奕迅），但对
// 冷门/独立/古风/网友改编这类曲目覆盖不全；酷狗恰好相反——付费曲
// 拿不到（返回 "The Resource Needs to be Paid"），但免费曲库极厚。
// 两者互补，加起来基本能覆盖固件能点到的绝大多数歌名。
//
// 重要限制：
//  1) 搜索接口只走 **http**。NAS 上 mobilecdn.kugou.com 解析到 IPv6，
//     https 握手直接失败（curl 返回 000），http 反而正常。
//  2) pay_type==3 是付费曲，取链必然失败，直接跳过，不要浪费 3 秒超时。
//     周杰伦这类整批付费的歌手，本源搜索完立刻返回空，把机会让给酷我。
//  3) 酷狗免费曲里「网友改编 / DJ版 / Live / 伴奏」占比很高，且 pay_type
//     全是 0。如果打分不严，点《起风了》会被换成《起风了 (网友改编)》，
//     因此噪声判定必须基于**原始标题**（gdNorm 会把括号内容整段删掉，
//     噪声信息就丢了）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// kgBlocked 表示酷狗整体不可用（接口改版/被封），置位后本进程内跳过。
var kgBlocked atomic.Bool

// kgClient 有两个刻意的设置，都是被 NAS 网络环境逼出来的：
//
//  1. 强制优先 IPv4。mobilecdn.kugou.com 在 NAS 上只解析出 IPv6，
//     而该机器的 IPv6 出口是坏的——https 握手直接超时（curl 返回 000），
//     http 走 IPv6 却能通。用 tcp4 先试、失败再回落 tcp6，两条路都保住。
//  2. 不跟随重定向。Go 默认行为会把 http://m.kugou.com/... 301 到 https，
//     然后卡在坏掉的 https 上，表现为「搜索有结果但取链永远失败」。
//     curl 不加 -L 能直接拿到 JSON，所以这里也照做。
var kgClient = &http.Client{
	Timeout: 9 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
			conn, err := d.DialContext(ctx, "tcp4", addr)
			if err == nil {
				return conn, nil
			}
			return d.DialContext(ctx, "tcp6", addr)
		},
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type kgSong struct {
	Hash      string
	Name      string
	Artist    string
	Album     string
	Duration  int
	PayType   int
	Privilege int
	Original  bool // isoriginal==1，酷狗自家标记的原唱
	score     int
	fatal     bool
	noVocal   bool
}

type kgSearchResp struct {
	Status int `json:"status"`
	Data   struct {
		Total int `json:"total"`
		Info  []struct {
			Hash       string `json:"hash"`
			SongName   string `json:"songname"`
			SingerName string `json:"singername"`
			AlbumName  string `json:"album_name"`
			Duration   int    `json:"duration"`
			FileSize   int    `json:"filesize"`
			PayType    int    `json:"pay_type"`
			Privilege  int    `json:"privilege"`
			IsOriginal int    `json:"isoriginal"`
			ExtName    string `json:"extname"`
		} `json:"info"`
	} `json:"data"`
}

func kgRequest(rawURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	resp, err := kgClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func kgSearch(song, singer string) []kgSong {
	return kgSearchLimit(song, singer, 15)
}

// kgSearchLimit 按需拉取指定条数。酷狗单页 pagesize 实测封顶 30，
// 需要更多时按 page 逐页翻，最多 5 页(150 首)。
func kgSearchLimit(song, singer string, limit int) []kgSong {
	if limit <= 0 {
		limit = 15
	}
	if limit > 150 {
		limit = 150
	}
	keyword := strings.TrimSpace(song + " " + singer)
	out := make([]kgSong, 0, limit)
	for page := 1; len(out) < limit; page++ {
		items := kgSearchPage(keyword, page, 30)
		if len(items) == 0 {
			break
		}
		out = append(out, items...)
		if len(items) < 30 {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func kgSearchPage(keyword string, page, pagesize int) []kgSong {
	api := "http://mobilecdn.kugou.com/api/v3/search/song?format=json&keyword=" +
		url.QueryEscape(keyword) + "&page=" + strconv.Itoa(page) +
		"&pagesize=" + strconv.Itoa(pagesize) + "&showtype=1"

	t0 := time.Now()
	body, err := kgRequest(api)
	if err != nil {
		fmt.Printf("[Kugou] search error: %v (%v)\n", err, time.Since(t0).Round(time.Millisecond))
		return nil
	}
	var resp kgSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[Kugou] search json error: %v\n", err)
		return nil
	}
	if resp.Status != 1 {
		fmt.Printf("[Kugou] search status=%d\n", resp.Status)
		return nil
	}
	fmt.Printf("[Kugou] search(%q) page=%d -> %d hits (%v)\n", keyword, page, len(resp.Data.Info), time.Since(t0).Round(time.Millisecond))

	out := make([]kgSong, 0, len(resp.Data.Info))
	for _, it := range resp.Data.Info {
		out = append(out, kgSong{
			Hash:      it.Hash,
			Name:      it.SongName,
			Artist:    it.SingerName,
			Album:     it.AlbumName,
			Duration:  it.Duration,
			PayType:   it.PayType,
			Privilege: it.Privilege,
			Original:  it.IsOriginal == 1,
		})
	}
	return out
}

func kgScoreTrack(t *kgSong, song, singer string) (int, bool) {
	score := 0
	fatal := false

	wantName := kwNormName(song)
	gotName := kwNormName(t.Name)
	ts, tfatal := srcTitleScore(gotName, wantName)
	score += ts
	if tfatal {
		fatal = true
	}

	// 歌手维度
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

	// 版本噪声必须看原始标题：归一化会把「(网友改编)」整段去掉
	if pen, fat, nv := srcNoiseOf(t.Name, t.Album, song); fat {
		score += pen
		fatal = true
		t.noVocal = nv
	}

	// 时长：1 分钟以下基本是片段
	if t.Duration > 0 && t.Duration < 60 {
		score -= 80
		fatal = true
	}
	// 酷狗自家标记的原唱，给一点加权（用于同分排序）
	if t.Original {
		score += 8
	}
	return score, fatal
}

// kgRank 打分排序。同分时优先「时长更接近中位数」的候选：
// 原版通常就是被发得最多的那一版，而不是最长的那一版。
func kgRank(list []kgSong, song, singer string) []kgSong {
	for i := range list {
		list[i].score, list[i].fatal = kgScoreTrack(&list[i], song, singer)
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

// kgPlayURL 用 hash 换完整音频直链。取不到返回空串。
// 免费曲返回 sharefs.kugou.com 的 mp3，付费曲返回 "The Resource Needs to be Paid"。
func kgPlayURL(hash string) (string, int) {
	api := "http://m.kugou.com/app/i/getSongInfo.php?cmd=playInfo&hash=" + hash
	body, err := kgRequest(api)
	if err != nil {
		fmt.Printf("[Kugou] playInfo error: %v\n", err)
		return "", 0
	}
	var resp struct {
		URL        string      `json:"url"`
		BackupURL  interface{} `json:"backup_url"`
		TimeLength int         `json:"timeLength"`
		ErrCode    int         `json:"errcode"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[Kugou] playInfo json error: %v (%.100s)\n", err, string(body))
		return "", 0
	}
	if strings.HasPrefix(resp.URL, "http") {
		return resp.URL, resp.TimeLength
	}
	// backup_url 有时是字符串，有时是数组
	switch v := resp.BackupURL.(type) {
	case string:
		if strings.HasPrefix(v, "http") {
			return v, resp.TimeLength
		}
	case []interface{}:
		for _, x := range v {
			if s, ok := x.(string); ok && strings.HasPrefix(s, "http") {
				return s, resp.TimeLength
			}
		}
	}
	return "", 0
}

// KugouSearch 对外入口。只试免费曲（pay_type!=3），付费曲不浪费时间。
func KugouSearch(song, singer string) MusicItem {
	if strings.TrimSpace(song) == "" || kgBlocked.Load() {
		fmt.Printf("[Kugou] skip (empty song or blocked=%v)\n", kgBlocked.Load())
		return MusicItem{}
	}
	list := kgSearch(song, singer)
	if len(list) == 0 {
		fmt.Printf("[Kugou] no result for %q - %q\n", song, singer)
		return MusicItem{}
	}
	if len(list) > 12 {
		list = list[:12]
	}
	ranked := kgRank(list, song, singer)

	var queue []kgSong
	for _, t := range ranked {
		if !t.fatal && t.score >= 40 && t.PayType != 3 {
			queue = append(queue, t)
		}
	}
	// 注意：这里**不做**「没有原唱就退而求其次用现场版/改编版」的兜底。
	// 实测放开后点《成都》会放成「(2026没有信号巡回演唱会北京站)」、
	// 点《大鱼》放成「(3D环绕版)」，用户一听就知道错了。宁可这里返回空，
	// 让后面的源去试真正的原唱；所有源都没有时，最后的 GDStudio / yuafeng
	// 那一层保留了宽容策略，兜底不至于断歌。
	if len(queue) == 0 {
		fmt.Printf("[Kugou] 无可用免费候选 for %q - %q\n", song, singer)
		return MusicItem{}
	}
	if len(queue) > 4 {
		queue = queue[:4]
	}

	for i, t := range queue {
		if i < 3 {
			fmt.Printf("[Kugou] cand #%d score=%d pay=%d priv=%d %q - %q (%ds hash=%s)\n",
				i+1, t.score, t.PayType, t.Privilege, t.Name, t.Artist, t.Duration, t.Hash)
		}
		audioURL, dur := kgPlayURL(t.Hash)
		if audioURL == "" {
			continue
		}
		if dur <= 0 {
			dur = t.Duration
		}
		fmt.Printf("[Kugou] HIT #%d %q - %q (%ds, score=%d)\n", i+1, t.Name, t.Artist, dur, t.score)
		return MusicItem{
			Title:        t.Name,
			Artist:       t.Artist,
			AudioURL:     audioURL,
			AudioFullURL: audioURL,
			Duration:     dur,
		}
	}
	fmt.Printf("[Kugou] all candidates unusable for %q - %q\n", song, singer)
	return MusicItem{}
}
