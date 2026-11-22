package main

// 歌单搬运：导出 m3u / 导入网易云歌单
//
// 为什么做这个：这套服务最大的用处是「自己的曲库 + 自己的音箱」，
// 但歌单一进一出都得靠手点，换设备、换播放器、备份都很麻烦。
//
//   GET  /api/user/playlists/export?id=<歌单ID>&format=m3u|txt|json
//   POST /api/user/playlists/import-netease  {"url":"https://music.163.com/playlist?id=xxx"}
//
// 导出的 m3u 里每首歌指向本机 /stream_live，VLC / foobar / 手机播放器
// 直接打开就能按歌单顺序播（服务器会按需转码，跟网页播放是同一条链路）。
// 网易云歌单导入只取「歌名 + 歌手 + 时长」，播放仍然走多源回退，
// 所以导入的歌单没有版权限制问题。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// publicBaseURL 生成对外可访问的地址：优先 PUBLIC_BASE_URL（反代/域名场景），
// 否则按请求本身推断。导出的 m3u 要被别的播放器打开，必须是绝对地址。
func publicBaseURL(r *http.Request) string {
	if v := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func contentDispositionAttachment(filename string) string {
	ascii := strings.Map(func(c rune) rune {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return '_'
		}
		return c
	}, filename)
	return fmt.Sprintf("attachment; filename=\"%s\"; filename*=UTF-8''%s",
		ascii, url.PathEscape(filename))
}

// HandleExportPlaylist 导出歌单。m3u 是默认格式，另外支持 txt / json 便于脚本处理。
func HandleExportPlaylist(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	name := strings.TrimSpace(q.Get("name"))
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "m3u"
	}

	var pl *UserPlaylist
	if id != "" {
		p, err := playlistManager.GetUserPlaylistByID(userID, id)
		if err != nil {
			http.Error(w, "歌单不存在", http.StatusNotFound)
			return
		}
		pl = p
	} else if name != "" {
		for _, p := range playlistManager.GetUserPlaylists(userID) {
			if p.Name == name {
				pl = p
				break
			}
		}
		if pl == nil {
			http.Error(w, "歌单不存在", http.StatusNotFound)
			return
		}
	} else {
		http.Error(w, "缺少 id 或 name 参数", http.StatusBadRequest)
		return
	}

	w.Header().Set("Cache-Control", "no-store")

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(pl)
		return
	}

	base := publicBaseURL(r)
	var b strings.Builder

	switch format {
	case "txt":
		for _, s := range pl.Songs {
			label := strings.TrimSpace(s.Title)
			if a := strings.TrimSpace(s.Artist); a != "" {
				label = a + " - " + label
			}
			b.WriteString(label + "\n")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment(pl.Name+".txt"))
	default: // m3u
		b.WriteString("#EXTM3U\n")
		b.WriteString("#PLAYLIST:" + pl.Name + "\n")
		for _, s := range pl.Songs {
			title := strings.TrimSpace(s.Title)
			if title == "" {
				continue
			}
			artist := strings.TrimSpace(s.Artist)
			label := title
			if artist != "" {
				label = artist + " - " + title
			}
			dur := s.Duration
			if dur <= 0 {
				dur = cachedDuration(title, artist)
			}
			if dur <= 0 {
				dur = -1 // m3u 约定：未知时长写 -1
			}
			fmt.Fprintf(&b, "#EXTINF:%d,%s\n%s%s\n", dur, label, base, livePlayURL(title, artist))
		}
		w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment(pl.Name+".m3u"))
	}

	fmt.Printf("[Playlist] 导出 %q（%d 首，%s）\n", pl.Name, len(pl.Songs), format)
	w.Write([]byte(b.String()))
}

// ---------- 网易云歌单导入 ----------

type neTrack struct {
	Name string `json:"name"`
	Ar   []struct {
		Name string `json:"name"`
	} `json:"ar"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Dt       int `json:"dt"`
	Duration int `json:"duration"`
	Al       struct {
		PicURL string `json:"picUrl"`
	} `json:"al"`
}

type nePlaylistDetail struct {
	Code     int `json:"code"`
	Playlist struct {
		Name       string    `json:"name"`
		CoverURL   string    `json:"coverImgUrl"`
		TrackCount int       `json:"trackCount"`
		Tracks     []neTrack `json:"tracks"`
	} `json:"playlist"`
}

var (
	neIDQueryRe = regexp.MustCompile(`[?&#]id=(\d+)`)
	neIDPathRe  = regexp.MustCompile(`/playlist/(\d+)`)
	digitsRe    = regexp.MustCompile(`^\d{3,}$`)
)

// extractNeteasePlaylistID 支持三种输入：纯数字 ID、分享链接、带 id= 的链接。
func extractNeteasePlaylistID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if digitsRe.MatchString(raw) {
		return raw
	}
	if m := neIDQueryRe.FindStringSubmatch(raw); len(m) == 2 {
		return m[1]
	}
	if m := neIDPathRe.FindStringSubmatch(raw); len(m) == 2 {
		return m[1]
	}
	return ""
}

// fetchNeteasePlaylist 拉歌单详情。
// 用 v1 接口 + n=1000：不传 n 时只返回前 10 首（实测超出的会静默丢掉）。
func fetchNeteasePlaylist(id string, limit int) (string, []MusicItem, int, error) {
	apiURL := "https://music.163.com/api/v1/playlist/detail?id=" + url.QueryEscape(id) + "&n=1000"

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return "", nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) MeowMusic/1.0")
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("Cookie", "os=pc; appver=8.9.70")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, 0, fmt.Errorf("请求网易云失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, 0, fmt.Errorf("网易云返回 HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", nil, 0, fmt.Errorf("读取网易云响应失败: %v", err)
	}

	var detail nePlaylistDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return "", nil, 0, fmt.Errorf("解析网易云响应失败: %v", err)
	}
	if strings.TrimSpace(detail.Playlist.Name) == "" {
		return "", nil, 0, fmt.Errorf("歌单不存在、已设为私密或需要登录")
	}

	songs := make([]MusicItem, 0, len(detail.Playlist.Tracks))
	for _, t := range detail.Playlist.Tracks {
		if limit > 0 && len(songs) >= limit {
			break
		}
		title := strings.TrimSpace(t.Name)
		if title == "" {
			continue
		}
		var artists []string
		for _, a := range t.Ar {
			if n := strings.TrimSpace(a.Name); n != "" {
				artists = append(artists, n)
			}
		}
		if len(artists) == 0 {
			for _, a := range t.Artists {
				if n := strings.TrimSpace(a.Name); n != "" {
					artists = append(artists, n)
				}
			}
		}
		artist := strings.Join(artists, "/")

		ms := t.Dt
		if ms <= 0 {
			ms = t.Duration
		}
		playURL := livePlayURL(title, artist)
		songs = append(songs, MusicItem{
			Title:        title,
			Artist:       artist,
			AudioURL:     playURL,
			AudioFullURL: playURL,
			Duration:     normalizeDuration(ms),
			CoverURL:     strings.TrimSpace(t.Al.PicURL),
			LyricURL: "/api/lyric?song=" + url.QueryEscape(title) +
				"&singer=" + url.QueryEscape(artist) + "&format=lrc",
		})
	}

	return detail.Playlist.Name, songs, detail.Playlist.TrackCount, nil
}

// HandleImportNeteasePlaylist 把网易云歌单导入到自己的歌单（或新建一个）。
func HandleImportNeteasePlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持 POST"})
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未登录"})
		return
	}

	var req struct {
		URL        string `json:"url"`
		ID         string `json:"id"`
		TargetID   string `json:"target_id"`
		TargetName string `json:"target_name"`
		Limit      int    `json:"limit"`
		AutoSync   bool   `json:"auto_sync"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求格式错误"})
		return
	}

	raw := strings.TrimSpace(req.ID)
	if raw == "" {
		raw = strings.TrimSpace(req.URL)
	}
	id := extractNeteasePlaylistID(raw)
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "认不出歌单 ID，请粘贴歌单链接或纯数字 ID",
		})
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}

	name, songs, total, err := fetchNeteasePlaylist(id, limit)
	if err != nil {
		fmt.Printf("[Import] 网易云歌单 %s 拉取失败: %v\n", id, err)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	targetID := strings.TrimSpace(req.TargetID)
	if targetID == "" {
		plName := strings.TrimSpace(req.TargetName)
		if plName == "" {
			plName = "网易云 · " + name
		}
		created, cerr := playlistManager.CreateUserPlaylist(userID, plName, "导入自网易云歌单 "+id)
		if cerr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": cerr.Error()})
			return
		}
		targetID = created.ID
	} else if _, gerr := playlistManager.GetUserPlaylistByID(userID, targetID); gerr != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "目标歌单不存在"})
		return
	}

	added, skipped, aerr := playlistManager.AddSongsToUserPlaylist(userID, targetID, songs)
	if aerr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": aerr.Error()})
		return
	}

	fmt.Printf("[Import] 网易云歌单 %q(%s) -> 歌单 %s：新增 %d，跳过重复 %d\n",
		name, id, targetID, added, skipped)

	// 勾了「每天自动同步」就登记一条同步关系，之后每天只补新歌
	synced := false
	if req.AutoSync {
		job := registerSyncJob(userID, targetID, id, name)
		if job != nil {
			syncJobs.mu.Lock()
			job.Added = added
			job.Total = total
			job.LastAdded = added
			syncJobs.dirty = true
			syncJobs.saveLocked(true)
			syncJobs.mu.Unlock()
			synced = true
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"playlist_id": targetID,
		"source_name": name,
		"source_id":   id,
		"total":       total,
		"fetched":     len(songs),
		"added":       added,
		"skipped":     skipped,
		"auto_sync":   synced,
	})
}
