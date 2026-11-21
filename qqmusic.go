package main

// QQ 音乐通道：第三顺位兜底，覆盖 QQ 独占版权的免费曲。
//
// 实测结论（从 NAS 广州移动出口验证）：
//   - 搜索 c.y.qq.com/soso 正常，返回 songmid / interval / pay.pay_play，
//     原版时长完全对得上（稻香 223s、晴天 269s、海阔天空 324s）。
//   - 取链 u.y.qq.com music.vkey.GetVkey 无登录态也能拿到 vkey，但
//     **只有 pay_play==0 的免费曲 purl 才非空**。周杰伦这类数字专辑
//     （pay_play==1）返 purl 为空，本源对它们无能为力——交给酷我。
//   - 免费曲能拿到完整 m4a（实测 2.6MB / 213s，98116bps），ffmpeg 可直接读。
//
// 因此本源不追求覆盖大牌，只负责「酷我 + 酷狗 + 网易云都没有，但 QQ 有」
// 的那部分曲目（独立音乐、ACG、部分翻唱授权版）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var qqBlocked atomic.Bool

var qqClient = &http.Client{Timeout: 9 * time.Second}

// qqGUID 取链请求里的设备号，整个进程固定即可
var qqGUID = "10000"

type qqSong struct {
	Mid      string
	Name     string
	Artist   string
	Album    string
	Duration int
	PayPlay  int
	score    int
	fatal    bool
	noVocal  bool
}

type qqSearchResp struct {
	Code int `json:"code"`
	Data struct {
		Song struct {
			List []struct {
				Mid      string `json:"mid"`
				SongMid  string `json:"songmid"`
				Title    string `json:"title"`
				SongName string `json:"songname"`
				Interval int    `json:"interval"`
				Singer   []struct {
					Name string `json:"name"`
				} `json:"singer"`
				Album struct {
					Name string `json:"name"`
				} `json:"album"`
				AlbumName string `json:"albumname"`
				Pay       struct {
					PayPlay int `json:"pay_play"`
				} `json:"pay"`
			} `json:"list"`
		} `json:"song"`
	} `json:"data"`
}

func qqRequest(rawURL, method string, body []byte, referer string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", neUA)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "*/*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := qqClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func qqSearch(song, singer string) []qqSong {
	return qqSearchLimit(song, singer, 15)
}

// qqSearchLimit 按需拉取指定条数。实测 n 最大 60（n>60 会被压回 60，
// n=100 直接返回 0 条），所以这里封顶 60。
func qqSearchLimit(song, singer string, limit int) []qqSong {
	if limit <= 0 {
		limit = 15
	}
	if limit > 60 {
		limit = 60
	}
	keyword := strings.TrimSpace(song + " " + singer)
	// 参数集是实测出来的：带上 remoteplace=txt.yqq.song 或 searchid=0 时，
	// 接口会稳定返回 0 条（curnum:0），去掉这两个后同样的关键词能返回 15 条。
	// 不要"补全"成官方网页用的一堆参数，会把结果清零。
	api := "https://c.y.qq.com/soso/fcgi-bin/client_search_cp?new_json=1&cr=1&p=1&n=" +
		strconv.Itoa(limit) + "&format=json&w=" + url.QueryEscape(keyword)

	body, err := qqRequest(api, "GET", nil, "https://y.qq.com/")
	if err != nil {
		fmt.Printf("[QQMusic] search error: %v\n", err)
		return nil
	}
	var resp qqSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[QQMusic] search json error: %v (%.80s)\n", err, string(body))
		return nil
	}
	out := make([]qqSong, 0, len(resp.Data.Song.List))
	for _, it := range resp.Data.Song.List {
		mid := it.Mid
		if mid == "" {
			mid = it.SongMid
		}
		name := it.Title
		if name == "" {
			name = it.SongName
		}
		names := make([]string, 0, len(it.Singer))
		for _, s := range it.Singer {
			names = append(names, s.Name)
		}
		album := it.Album.Name
		if album == "" {
			album = it.AlbumName
		}
		out = append(out, qqSong{
			Mid:      mid,
			Name:     name,
			Artist:   strings.Join(names, "/"),
			Album:    album,
			Duration: it.Interval,
			PayPlay:  it.Pay.PayPlay,
		})
	}
	return out
}

func qqScoreTrack(t *qqSong, song, singer string) (int, bool) {
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

func qqRank(list []qqSong, song, singer string) []qqSong {
	for i := range list {
		list[i].score, list[i].fatal = qqScoreTrack(&list[i], song, singer)
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

// qqVkeyReq 批量查询 vkey：一次最多送 10 个 mid，省掉多轮往返
type qqVkeyReq struct {
	Req0 struct {
		Module string `json:"module"`
		Method string `json:"method"`
		Param  struct {
			GUID       string   `json:"guid"`
			SongMid    []string `json:"songmid"`
			SongType   []int    `json:"songtype"`
			Uin        string   `json:"uin"`
			LoginFlag  int      `json:"loginflag"`
			Platform   string   `json:"platform"`
		} `json:"param"`
	} `json:"req_0"`
	Comm struct {
		Uin    int    `json:"uin"`
		Format string `json:"format"`
		Ct     int    `json:"ct"`
		Cv     int    `json:"cv"`
	} `json:"comm"`
}

type qqVkeyResp struct {
	Code int `json:"code"`
	Req0 struct {
		Code int `json:"code"`
		Data struct {
			Sip        []string `json:"sip"`
			Midurlinfo []struct {
				SongMid string `json:"songmid"`
				Purl    string `json:"purl"`
			} `json:"midurlinfo"`
		} `json:"data"`
	} `json:"req_0"`
}

// qqVkeys 批量取 purl，返回 mid -> 完整可播放 URL
func qqVkeys(mids []string) map[string]string {
	out := map[string]string{}
	if len(mids) == 0 {
		return out
	}
	if len(mids) > 10 {
		mids = mids[:10]
	}
	var req qqVkeyReq
	req.Req0.Module = "vkey.GetVkeyServer"
	req.Req0.Method = "CgiGetVkey"
	req.Req0.Param.GUID = qqGUID
	req.Req0.Param.SongMid = mids
	req.Req0.Param.SongType = make([]int, len(mids))
	req.Req0.Param.Uin = "0"
	req.Req0.Param.LoginFlag = 1
	req.Req0.Param.Platform = "20"
	req.Comm.Uin = 0
	req.Comm.Format = "json"
	req.Comm.Ct = 24
	req.Comm.Cv = 0

	body, err := json.Marshal(req)
	if err != nil {
		return out
	}
	respBody, err := qqRequest("https://u.y.qq.com/cgi-bin/musicu.fcg", "POST", body, "https://y.qq.com/")
	if err != nil {
		fmt.Printf("[QQMusic] vkey error: %v\n", err)
		return out
	}
	var r qqVkeyResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		fmt.Printf("[QQMusic] vkey json error: %v\n", err)
		return out
	}
	sip := ""
	if len(r.Req0.Data.Sip) > 0 {
		sip = r.Req0.Data.Sip[0]
	}
	for _, m := range r.Req0.Data.Midurlinfo {
		if m.Purl == "" {
			continue
		}
		u := m.Purl
		if !strings.HasPrefix(u, "http") {
			u = sip + u
		}
		out[m.SongMid] = u
	}
	return out
}

// QQMusicSearch 对外入口。免费曲（pay_play==0）才取链，付费曲跳过。
func QQMusicSearch(song, singer string) MusicItem {
	if strings.TrimSpace(song) == "" || qqBlocked.Load() {
		return MusicItem{}
	}
	list := qqSearch(song, singer)
	if len(list) == 0 {
		fmt.Printf("[QQMusic] no result for %q - %q\n", song, singer)
		return MusicItem{}
	}
	fmt.Printf("[QQMusic] search -> %d hits\n", len(list))
	if len(list) > 12 {
		list = list[:12]
	}
	ranked := qqRank(list, song, singer)

	var queue []qqSong
	for _, t := range ranked {
		if !t.fatal && t.score >= 40 && t.PayPlay == 0 {
			queue = append(queue, t)
		}
	}
	// 注意：这里**不做**「没有原唱就退而求其次用现场版/改编版」的兜底。
	// 实测放开后点《成都》会放成「(2026没有信号巡回演唱会北京站)」、
	// 点《大鱼》放成「(3D环绕版)」，用户一听就知道错了。宁可这里返回空，
	// 让后面的源去试真正的原唱；所有源都没有时，最后的 GDStudio / yuafeng
	// 那一层保留了宽容策略，兜底不至于断歌。
	if len(queue) == 0 {
		fmt.Printf("[QQMusic] 无可用免费候选 for %q - %q\n", song, singer)
		return MusicItem{}
	}
	if len(queue) > 4 {
		queue = queue[:4]
	}

	mids := make([]string, 0, len(queue))
	for _, t := range queue {
		mids = append(mids, t.Mid)
	}
	urls := qqVkeys(mids)

	for i, t := range queue {
		if i < 2 {
			fmt.Printf("[QQMusic] cand #%d score=%d pay=%d %q - %q (%ds)\n",
				i+1, t.score, t.PayPlay, t.Name, t.Artist, t.Duration)
		}
		u := urls[t.Mid]
		if u == "" {
			continue
		}
		fmt.Printf("[QQMusic] HIT #%d %q - %q (%ds, score=%d)\n", i+1, t.Name, t.Artist, t.Duration, t.score)
		return MusicItem{
			Title:        t.Name,
			Artist:       t.Artist,
			AudioURL:     u,
			AudioFullURL: u,
			Duration:     t.Duration,
		}
	}
	fmt.Printf("[QQMusic] all candidates unusable for %q - %q\n", song, singer)
	return MusicItem{}
}
