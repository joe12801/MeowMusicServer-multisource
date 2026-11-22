package main

// 5sing（5sing.kugou.com）—— 原创 / 翻唱 / 伴奏社区
//
// 定位：和喜马拉雅一样是**只在自身筛选按钮下出现**的补充源。
// 它的内容全是用户上传的原创、翻唱、伴奏，和唱片公司的曲库不重叠；
// 混进「全部」会把搜索结果冲成一堆翻唱，所以不参与 getRemoteMusicURL 的
// 多源链路，也不进「全部」搜索。
//
// 走网页版公开接口（2026-11-23 在东京机器实测：搜索 200、取链 200、直链 206 audio/mpeg）：
//
//	搜索  GET http://search.5sing.kugou.com/home/json?keyword=<kw>&sort=1&page=<n>&filter=0&type=0
//	      → list[]：songId / songName（含 <em class="keyword"> 高亮标签）/ singer /
//	        typeEname（yc 原创、fc 翻唱、bz 伴奏）；pageInfo.totalPages 控制翻页
//	取链  GET http://mobileapi.5sing.kugou.com/song/getSongUrl?songid=<id>&songtype=<type>
//	      → data.squrl / hqurl / lqurl（mp3，https 直链），另有 *_backup 备用域名
//
// 两个刻意的实现决定：
//  1. 搜索域名 search.5sing.kugou.com 的证书只签给 *.kugou.com，https 直接
//     握手失败（Hostname mismatch），所以搜索走 http——返回的是公开 JSON，没有凭据。
//  2. 搜索列表里没有 duration，只能留 0 交给 cachedDuration 兜底；封面也没有，
//     getSongUrl 返回的 user.I 是上传者头像，不是专辑图，不拿来当封面。

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	sourceFiveSing = "fivesing"

	// 播放地址里携带的 xid 形如 fs:<songId>:<typeEname>，
	// 因为 5sing 取链必须同时给 songid 和 songtype（yc/fc/bz），只给 id 取不到。
	fiveSingXIDPrefix = "fs:"

	fiveSingSearchAPI = "http://search.5sing.kugou.com/home/json"
	fiveSingURLAPI    = "http://mobileapi.5sing.kugou.com/song/getSongUrl"
	fiveSingReferer   = "https://5sing.kugou.com/"
	fiveSingUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// 单次搜索最多翻这么多页（每页 10 条），limit 大时也不至于把站点扫穿
	fiveSingMaxPages = 5
)

// fsTrack 是 5sing 的一首作品
type fsTrack struct {
	ID       string
	Type     string // yc 原创 / fc 翻唱 / bz 伴奏
	Name     string
	Artist   string
	PlayCnt  int
	DownCnt  int
}

var (
	fiveSingClient = &http.Client{Timeout: 15 * time.Second}
	fsTagRe        = regexp.MustCompile(`<[^>]*>`)
)

// fivesingEnabled 开关：FIVESING_ENABLED=0 关掉（默认开）
func fivesingEnabled() bool {
	v := strings.TrimSpace(os.Getenv("FIVESING_ENABLED"))
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "off")
}

func fsGet(rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fiveSingUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", fiveSingReferer)
	resp, err := fiveSingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return body, nil
}

// fsCleanName 去掉搜索接口塞进歌名里的 <em class="keyword"> 高亮标签，并还原实体。
func fsCleanName(s string) string {
	s = fsTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

type fsSearchResp struct {
	List []struct {
		SongID    json.Number `json:"songId"` // 有时是数字、有时是字符串
		SongName  string      `json:"songName"`
		Singer    string      `json:"singer"`
		TypeEname string      `json:"typeEname"`
		PlayCnt   int         `json:"playCnt"`
		DownCnt   int         `json:"downloadCnt"`
	} `json:"list"`
	PageInfo struct {
		Cur        int `json:"cur"`
		TotalPages int `json:"totalPages"`
	} `json:"pageInfo"`
}

func fsSearchRaw(keyword string, limit int) []fsTrack {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || limit <= 0 {
		return nil
	}
	if limit > 50 {
		limit = 50
	}

	out := make([]fsTrack, 0, limit)
	for page := 1; page <= fiveSingMaxPages; page++ {
		if len(out) >= limit {
			break
		}
		q := url.Values{}
		q.Set("keyword", keyword)
		q.Set("sort", "1")
		q.Set("page", strconv.Itoa(page))
		q.Set("filter", "0")
		q.Set("type", "0")

		body, err := fsGet(fiveSingSearchAPI + "?" + q.Encode())
		if err != nil {
			fmt.Printf("[fivesing] 搜索失败 %q 第 %d 页: %v\n", keyword, page, err)
			break
		}
		var resp fsSearchResp
		if err := json.Unmarshal(body, &resp); err != nil {
			fmt.Printf("[fivesing] 搜索响应解析失败 %q: %v\n", keyword, err)
			break
		}
		if len(resp.List) == 0 {
			break
		}
		for _, it := range resp.List {
			id := strings.TrimSpace(it.SongID.String())
			name := fsCleanName(it.SongName)
			typ := strings.TrimSpace(it.TypeEname)
			if id == "" || id == "0" || name == "" || typ == "" {
				continue
			}
			out = append(out, fsTrack{
				ID:      id,
				Type:    typ,
				Name:    name,
				Artist:  fsCleanName(it.Singer),
				PlayCnt: it.PlayCnt,
				DownCnt: it.DownCnt,
			})
			if len(out) >= limit {
				break
			}
		}
		if resp.PageInfo.TotalPages > 0 && page >= resp.PageInfo.TotalPages {
			break
		}
	}
	return out
}

// fiveSingTrackXID 拼播放用的 xid（fs:<songId>:<typeEname>）
func fiveSingTrackXID(songID, typ string) string {
	songID, typ = strings.TrimSpace(songID), strings.TrimSpace(typ)
	if songID == "" {
		return ""
	}
	if typ == "" {
		typ = "yc"
	}
	return fiveSingXIDPrefix + songID + ":" + typ
}

// FiveSingSearchLimit 供「5sing」单源搜索用。
//
// 搜索结果里会混进伴奏（typeEname=bz），而本站只播音频、不标「伴奏」，
// 用户点进去会听到一段没有人声的东西——默认按「排除伴奏」处理；
// 但如果整页都是伴奏（用户搜的就是「某某伴奏」），保留原样，别把人清空。
func FiveSingSearchLimit(song, singer string, limit int) []fsTrack {
	if !fivesingEnabled() {
		return nil
	}
	song, singer = strings.TrimSpace(song), strings.TrimSpace(singer)
	kw := strings.TrimSpace(song + " " + singer)
	list := fsSearchRaw(kw, limit)
	// 5sing 的检索偏「标题包含」，歌名+歌手拼一起常搜不到，退回只搜歌名
	if len(list) == 0 && singer != "" && song != "" {
		list = fsSearchRaw(song, limit)
	}
	if len(list) == 0 {
		return nil
	}

	kept := make([]fsTrack, 0, len(list))
	for _, t := range list {
		if t.Type == "bz" {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		kept = list
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return kept
}

type fsURLResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		SqURL       string `json:"squrl"`
		SqURLBackup string `json:"squrl_backup"`
		HqURL       string `json:"hqurl"`
		HqURLBackup string `json:"hqurl_backup"`
		LqURL       string `json:"lqurl"`
		LqURLBackup string `json:"lqurl_backup"`
		SqExt       string `json:"sqext"`
		SqSize      string `json:"sqsize"`
		HqSize      string `json:"hqsize"`
	} `json:"data"`
}

// FiveSingTrackSrc 按 xid（fs:<songId>:<typeEname>）取 m4a/mp3 直链。
// 取链必须同时给 songid 和 songtype，缺一个都返回空列表。
func FiveSingTrackSrc(xid string) (string, error) {
	xid = strings.TrimSpace(xid)
	xid = strings.TrimPrefix(xid, fiveSingXIDPrefix)
	parts := strings.SplitN(xid, ":", 2)
	songID := strings.TrimSpace(parts[0])
	songType := "yc"
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		songType = strings.TrimSpace(parts[1])
	}
	if songID == "" {
		return "", fmt.Errorf("缺少 5sing songId")
	}
	if _, err := strconv.ParseInt(songID, 10, 64); err != nil {
		return "", fmt.Errorf("5sing songId 格式不对: %s", songID)
	}

	q := url.Values{}
	q.Set("songid", songID)
	q.Set("songtype", songType)
	body, err := fsGet(fiveSingURLAPI + "?" + q.Encode())
	if err != nil {
		return "", err
	}
	var resp fsURLResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("取链响应解析失败: %w", err)
	}
	if resp.Code != 1000 {
		msg := strings.TrimSpace(resp.Message)
		if msg == "" {
			msg = fmt.Sprintf("code=%d", resp.Code)
		}
		return "", fmt.Errorf("5sing 取链失败: %s", msg)
	}

	// 优先无损（sq），其次 hq/lq；主域名不通再试 backup 域名（*.cloud.kugou.com）
	for _, u := range []string{
		resp.Data.SqURL, resp.Data.HqURL, resp.Data.LqURL,
		resp.Data.SqURLBackup, resp.Data.HqURLBackup, resp.Data.LqURLBackup,
	} {
		u = strings.TrimSpace(u)
		if u == "" || !strings.HasPrefix(u, "http") {
			continue
		}
		if strings.HasPrefix(u, "http://") {
			u = "https://" + strings.TrimPrefix(u, "http://")
		}
		return u, nil
	}
	return "", fmt.Errorf("该作品暂无可用播放地址（可能已下架或仅会员可听）")
}
