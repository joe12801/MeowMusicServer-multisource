package main

// 网易云音乐 API（NeteaseCloudMusicApiEnhanced）音源适配
// 服务地址默认 http://127.0.0.1:3000，可用环境变量 NCM_API_BASE 覆盖
//
// 相比 GD Studio 的优势：
//   1. 走官方搜索接口，返回的 artist 是真实歌手，能把原唱和翻唱区分开
//   2. /song/url/v1 带 unblock=true 时，会用 UnblockNeteaseMusic 从
//      酷我/酷狗/QQ/咪咕等源匹配同名歌曲，把「灰色/无版权」的歌也变成可播
//   缺点：网易云对完全下架的歌手（如周杰伦）不返回搜索结果，
//        连 id 都拿不到，解灰也就无从下手 —— 需要登录后才会返回。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 网易云 API 服务地址，默认本机 3000 端口；可用环境变量 NCM_API_BASE 覆盖
var ncmAPIBase = func() string {
	if v := os.Getenv("NCM_API_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:3000"
}()

type ncmArtist struct {
	Name string `json:"name"`
}

type ncmSong struct {
	ID      int64       `json:"id"`
	Name    string      `json:"name"`
	Artists []ncmArtist `json:"artists"`
	Ar      []ncmArtist `json:"ar"` // 老接口字段名
	Album   struct {
		Name string `json:"name"`
	} `json:"album"`
	Al struct {
		Name string `json:"name"`
	} `json:"al"`
	Duration int `json:"duration"`
	Dt       int `json:"dt"`
}

func (s *ncmSong) artistNames() []string {
	src := s.Artists
	if len(src) == 0 {
		src = s.Ar
	}
	out := make([]string, 0, len(src))
	for _, a := range src {
		if a.Name != "" {
			out = append(out, a.Name)
		}
	}
	return out
}

func (s *ncmSong) albumName() string {
	if s.Album.Name != "" {
		return s.Album.Name
	}
	return s.Al.Name
}

func (s *ncmSong) duration() int {
	if s.Duration > 0 {
		return s.Duration
	}
	return s.Dt
}

var ncmHttpClient = &http.Client{Timeout: 25 * time.Second}

func ncmGet(path string, params url.Values) ([]byte, error) {
	req, err := http.NewRequest("GET", ncmAPIBase+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := ncmHttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ncmSearchSongs 调 /cloudsearch 搜索歌曲
func ncmSearchSongs(keyword string, limit int) []ncmSong {
	p := url.Values{}
	p.Set("keywords", keyword)
	p.Set("type", "1")
	p.Set("limit", fmt.Sprintf("%d", limit))
	body, err := ncmGet("/cloudsearch", p)
	if err != nil {
		fmt.Printf("[NCM] search error: %v\n", err)
		return nil
	}
	var r struct {
		Result struct {
			Songs []ncmSong `json:"songs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		fmt.Printf("[NCM] search parse error: %v\n", err)
		return nil
	}
	return r.Result.Songs
}

// ncmSongURL 调 /song/url/v1，unblock=true 交给 UNM 去其他平台找同名歌
func ncmSongURL(id int64) (string, string) {
	for _, level := range []string{"exhigh", "standard"} {
		p := url.Values{}
		p.Set("id", fmt.Sprintf("%d", id))
		p.Set("level", level)
		p.Set("unblock", "true")
		body, err := ncmGet("/song/url/v1", p)
		if err != nil {
			fmt.Printf("[NCM] url(%d,%s) error: %v\n", id, level, err)
			continue
		}
		var r struct {
			Data []struct {
				URL  string `json:"url"`
				Type string `json:"type"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			continue
		}
		if len(r.Data) > 0 && strings.HasPrefix(r.Data[0].URL, "http") {
			return r.Data[0].URL, r.Data[0].Type
		}
	}
	return "", ""
}

// NCMSearch 搜索并返回可直接播放的 MusicItem
func NCMSearch(song, singer string) MusicItem {
	song = strings.TrimSpace(song)
	if song == "" {
		return MusicItem{}
	}
	keyword := song
	if singer != "" {
		keyword = song + " " + strings.TrimSpace(singer)
	}

	songs := ncmSearchSongs(keyword, 15)
	if len(songs) == 0 && singer != "" {
		// 带歌手搜不到时，退回只搜歌名
		songs = ncmSearchSongs(song, 15)
	}
	if len(songs) == 0 {
		fmt.Printf("[NCM] no result for %q\n", keyword)
		return MusicItem{}
	}
	fmt.Printf("[NCM] search %q -> %d hits\n", keyword, len(songs))

	// 复用 GD Studio 那套打分（转成统一结构）
	pool := make([]gdTrack, 0, len(songs))
	for i := range songs {
		ar := songs[i].artistNames()
		pool = append(pool, gdTrack{
			ID:     fmt.Sprintf("%d", songs[i].ID),
			Name:   songs[i].Name,
			Artist: ar,
			Album:  songs[i].albumName(),
		})
	}
	ranked := gdRankTracks(pool, song, singer, -30)
	if len(ranked) == 0 {
		ranked = pool
	}
	if len(ranked) > 3 {
		ranked = ranked[:3]
	}

	for i := range ranked {
		t := ranked[i]
		if i < 3 {
			fmt.Printf("[NCM] cand #%d score=%d %q - %q (%s)\n",
				i+1, t.score, t.Name, strings.Join(t.Artist, "/"), t.ID)
		}
		var id int64
		fmt.Sscanf(t.ID, "%d", &id)
		audioURL, format := ncmSongURL(id)
		if audioURL == "" {
			continue
		}
		fmt.Printf("[NCM] resolved %s - %s [%s]\n", t.Name, strings.Join(t.Artist, "/"), format)
		item := MusicItem{
			Title:        t.Name,
			Artist:       strings.Join(t.Artist, "/"),
			AudioURL:     audioURL,
			AudioFullURL: audioURL,
			CoverURL:     "",
			Duration:     0,
		}
		fmt.Printf("[NCM] HIT %q by %q (score=%d)\n", item.Title, item.Artist, t.score)
		return item
	}

	fmt.Printf("[NCM] no playable url for %q\n", keyword)
	return MusicItem{}
}
