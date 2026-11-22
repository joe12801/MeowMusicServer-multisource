package main

// 汽水音乐源（source = "qishui"）
//
// 汽水没有公开 API，音频还是加密的，所以不自己啃协议，而是复用同机的
// go-music-api 容器（镜像 guohuiyuan/go-music-api，只跑在容器网络里，
// 不映射宿主机端口）：
//
//	GET /api/v1/music/search?q=..&type=song&sources=soda   搜索
//	GET /api/v1/music/url?id=..&source=soda                 取明文直链（它负责解密）
//
// 本文件只做三件事：
//   1. 搜索并按「歌名 + 歌手 + 版本噪声 + 时长」打分，挑最像原唱的一首；
//   2. 给播放链路（helper.go 的 requestAndCacheMusic）提供 QishuiSearch；
//   3. 给搜索聚合（search.go）提供候选列表。
//
// 注意（2026-11-23 更新）：未登录现在也能拿到完整版（实测 194s 的歌直链 6.24MB、
// 约 257kbps），不再是早期的 30 秒试听。但直链由 CDN 就近调度，境外约 1/3 概率
// 落到不通的节点（v11-luna），所以转码失败要换节点重试，见 qishuiRefetchURL。
//
// 即便如此，它仍只排在国内主源之后、mp3api / yuafeng 之前兜底——
// 国内源命中率高、直链稳定，优先它们更划算。
//
// 配置：
//   QISHUI_BASE=http://gomusic:8080   容器地址（默认就是 compose 里的服务名）
//   QISHUI_ENABLED=0                  整路关掉

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const qishuiDefaultBase = "http://gomusic:8080"

// sourceQishui 音源标识。MusicItem.Source 用它标记「这条来自汽水」，
// /stream_live 靠它判断转码失败后能否换节点重试。
const sourceQishui = "qishui"

var qishuiClient = &http.Client{Timeout: 12 * time.Second}

func qishuiEnabled() bool {
	return strings.TrimSpace(os.Getenv("QISHUI_ENABLED")) != "0"
}

func qishuiBase() string {
	if v := strings.TrimSpace(os.Getenv("QISHUI_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return qishuiDefaultBase
}

// qishuiSong go-music-api 的 songs[] 元素（只取用得到的字段）。
type qishuiSong struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
	Cover    string `json:"cover"`
}

func qishuiGetJSON(raw string, out interface{}) error {
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0")
	resp, err := qishuiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("上游 HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// qishuiSearch 调 sidecar 搜索，只取汽水歌曲。
func qishuiSearch(keyword string, limit int) []qishuiSong {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil
	}
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	u := qishuiBase() + "/api/v1/music/search?q=" + url.QueryEscape(keyword) +
		"&type=song&sources=soda&limit=" + strconv.Itoa(limit)

	var wrap struct {
		Code int `json:"code"`
		Data struct {
			Songs []qishuiSong `json:"songs"`
		} `json:"data"`
	}
	if err := qishuiGetJSON(u, &wrap); err != nil {
		fmtLogf("[qishui] 搜索失败: %v", err)
		return nil
	}
	out := make([]qishuiSong, 0, len(wrap.Data.Songs))
	for _, s := range wrap.Data.Songs {
		if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.ID) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// qishuiTrackURL 取某首的明文播放地址（sidecar 会做解密）。
func qishuiTrackURL(id string) (string, error) {
	u := qishuiBase() + "/api/v1/music/url?id=" + url.QueryEscape(id) + "&source=soda"
	var wrap struct {
		Code int `json:"code"`
		Data struct {
			URL string `json:"url"`
			Ext string `json:"ext"`
		} `json:"data"`
	}
	if err := qishuiGetJSON(u, &wrap); err != nil {
		return "", err
	}
	link := strings.TrimSpace(wrap.Data.URL)
	if link == "" {
		return "", fmt.Errorf("直链为空")
	}
	return link, nil
}

// qishuiScoreTrack 打分：和酷狗/酷我同一套思路（歌名、歌手、版本噪声、时长）。
func qishuiScoreTrack(t *qishuiSong, song, singer string) (int, bool) {
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

	if pen, fat, _ := srcNoiseOf(t.Name, t.Album, song); fat {
		score += pen
		fatal = true
	}

	// 汽水自己的时长是整首的长度；太短的（<60s）多半是片段/翻唱切片，扣分排掉。
	if t.Duration > 0 && t.Duration < 60 {
		score -= 80
		fatal = true
	}
	return score, fatal
}

// qishuiPick 从候选里挑最匹配的一首，minScore 以下算没命中。
func qishuiPick(list []qishuiSong, song, singer string, minScore int) *qishuiSong {
	var best *qishuiSong
	bestScore := minScore
	for i := range list {
		s := &list[i]
		score, fatal := qishuiScoreTrack(s, song, singer)
		if fatal {
			continue
		}
		if score > bestScore {
			best, bestScore = s, score
		}
	}
	return best
}

// ---- 未登录兜底：公开榜单 ----
//
// 没登录时 /music/search 对汽水返回空（上游要登录态），但热门榜和官方榜单
// 是公开的。这里把榜单抓成一份「曲库快照」缓存 30 分钟，搜索先查接口，
// 接口没结果就从快照里按歌名/歌手匹配——至少抖音热歌能搜到、能放。

var qishuiRank struct {
	mu   sync.Mutex
	at   time.Time
	list []qishuiSong
}

func qishuiFetchSongList(path string) []qishuiSong {
	var wrap struct {
		Data struct {
			Songs []qishuiSong `json:"songs"`
		} `json:"data"`
	}
	if err := qishuiGetJSON(qishuiBase()+path, &wrap); err != nil {
		return nil
	}
	return wrap.Data.Songs
}

func qishuiRankSongs() []qishuiSong {
	qishuiRank.mu.Lock()
	defer qishuiRank.mu.Unlock()
	if len(qishuiRank.list) > 0 && time.Since(qishuiRank.at) < 30*time.Minute {
		return qishuiRank.list
	}

	seen := map[string]bool{}
	out := make([]qishuiSong, 0, 200)
	push := func(items []qishuiSong) {
		for _, s := range items {
			if strings.TrimSpace(s.ID) == "" || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			out = append(out, s)
		}
	}

	push(qishuiFetchSongList("/api/v1/ranking/hot?source=soda&limit=100"))

	var tl struct {
		Data struct {
			Sources []struct {
				Toplists []struct {
					ID string `json:"id"`
				} `json:"toplists"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := qishuiGetJSON(qishuiBase()+"/api/v1/ranking/toplists?sources=soda", &tl); err == nil {
		n := 0
		for _, src := range tl.Data.Sources {
			for _, t := range src.Toplists {
				if n >= 3 || strings.TrimSpace(t.ID) == "" {
					continue
				}
				push(qishuiFetchSongList("/api/v1/ranking/toplist?source=soda&toplist_id=" + url.QueryEscape(t.ID)))
				n++
			}
		}
	}

	if len(out) > 0 {
		qishuiRank.list, qishuiRank.at = out, time.Now()
	}
	return qishuiRank.list
}

// qishuiMatchRank 在榜单快照里按歌名/歌手找。
func qishuiMatchRank(song, singer string, limit int) []qishuiSong {
	if limit <= 0 {
		return nil
	}
	all := qishuiRankSongs()
	if len(all) == 0 {
		return nil
	}
	out := make([]qishuiSong, 0, limit)
	for i := range all {
		score, fatal := qishuiScoreTrack(&all[i], song, singer)
		if fatal || score < 60 {
			continue
		}
		out = append(out, all[i])
		if len(out) >= limit {
			break
		}
	}
	return out
}

// qishuiCandidates 统一入口：先接口搜索，没结果再落到榜单快照。
func qishuiCandidates(song, singer string, n int) []qishuiSong {
	keyword := strings.TrimSpace(song + " " + singer)
	if keyword == "" {
		return nil
	}
	list := qishuiSearch(keyword, n)
	if len(list) > 0 {
		return list
	}
	return qishuiMatchRank(song, singer, n)
}

// qishuiResolve 搜索 + 打分 + 取直链，是 QishuiSearch 和 qishuiRefetchURL 的公共部分。
// pick 用于日志/元数据，link 为空表示这次没取到可用直链。
func qishuiResolve(song, singer string) (*qishuiSong, string) {
	list := qishuiCandidates(song, singer, 10)
	if len(list) == 0 {
		return nil, ""
	}
	pick := qishuiPick(list, song, singer, 60)
	if pick == nil {
		return nil, ""
	}
	link, err := qishuiTrackURL(pick.ID)
	if err != nil {
		fmtLogf("[qishui] 取链失败 %s(%s): %v", pick.Name, pick.ID, err)
		return pick, ""
	}
	return pick, link
}

// qishuiRefetchURL 在转码失败后重新取一次直链。
//
// 汽水的直链由 CDN 就近调度，同一次取链每次请求可能落到不同节点。
// 境外实测：v3 / v5 / v5-ex / v26-luna 都可达（回归 6.2MB 全长、约 257kbps），
// 但 v11-luna 从东京整段超时，命中它就 ffmpeg 直接失败。
// 所以转码失败一次不要放弃，重取直链大概率换到可达节点。
func qishuiRefetchURL(song, singer string) string {
	pick, link := qishuiResolve(song, singer)
	if link == "" {
		return ""
	}
	fmtLogf("[qishui] 重新取链 %q - %q（换 CDN 节点重试）", pick.Name, pick.Artist)
	return link
}

// QishuiSearch 播放链路用：按「歌名 + 歌手」找到一首并取到直链。
func QishuiSearch(song, singer string) MusicItem {
	pick, link := qishuiResolve(song, singer)
	if pick == nil || link == "" {
		return MusicItem{}
	}
	fmtLogf("[qishui] 命中 %q - %q (%ds)", pick.Name, pick.Artist, pick.Duration)
	return MusicItem{
		Title:        pick.Name,
		Artist:       pick.Artist,
		AudioURL:     link,
		AudioFullURL: link,
		CoverURL:     pick.Cover,
		Duration:     pick.Duration,
		Source:       sourceQishui,
	}
}

// qishuiSearchLimit 搜索聚合用：返回候选（播放地址统一交给 /stream_live 再解析，
// 那是全局多源链路，别在这里绑死汽水）。
func qishuiSearchLimit(song, singer string, limit int) []qishuiSong {
	if limit <= 0 {
		return nil
	}
	list := qishuiCandidates(song, singer, limit*2)
	if len(list) == 0 {
		return nil
	}
	// 和搜索的其他源保持一致：能匹配上的优先，其余按上游顺序补充
	picked := make([]qishuiSong, 0, limit)
	used := make([]bool, len(list))
	fatal := make([]bool, len(list))
	scores := make([]int, len(list))
	for i := range list {
		scores[i], fatal[i] = qishuiScoreTrack(&list[i], song, singer)
	}
	// 第一轮：歌名/歌手对得上的，按分数从高到低挑
	for len(picked) < limit {
		bestIdx, bestScore := -1, 40
		for i := range list {
			if used[i] || fatal[i] {
				continue
			}
			if scores[i] > bestScore {
				bestIdx, bestScore = i, scores[i]
			}
		}
		if bestIdx < 0 {
			break
		}
		used[bestIdx] = true
		picked = append(picked, list[bestIdx])
	}
	// 第二轮：不够 limit 就按上游顺序补齐（只排掉明确无关的）。
	// 没有这一步，「按音源筛选」时上游 20 条只剩 4 条，看起来像搜不到歌。
	for i := range list {
		if len(picked) >= limit {
			break
		}
		if used[i] || fatal[i] {
			continue
		}
		used[i] = true
		picked = append(picked, list[i])
	}
	return picked
}

