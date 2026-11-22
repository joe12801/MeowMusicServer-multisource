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
// 另有一条完全绕开 sidecar 的取链路径（详见 qishuiShareURL）：直接扒网页版
// 分享页 music.douyin.com/qishui/share/track，从 _ROUTER_DATA 里读明文直链。
// 两个池的 CDN 节点分配互不相干，所以第二条不是替代，是「换池」兜底。
//
// 本文件做四件事：
//   1. 搜索并按「歌名 + 歌手 + 版本噪声 + 时长」打分，挑最像原唱的一首；
//   2. 给播放链路（helper.go 的 requestAndCacheMusic）提供 QishuiSearch；
//   3. 给搜索聚合（search.go）提供候选列表；
//   4. 双池取链 + 探活，见 qishuiPickURL。
//
// 内容长度（2026-11-23 修正）：早先记的「未登录也给全长」只对免费曲成立。
// 付费曲（分享页里 group_playable_level=pay）两个池都只给约 60 秒试听片段，
// 片段区间由 audioWithLyricsOption.previewStart/previewEnd 标出。实测「红色高跟鞋」
// 206s 的歌，两个池都只回 57.4s。这是上游版权策略，换路径解决不了。
//
// 直链由 CDN 就近调度，境外约 1/3~1/2 概率落到不通的节点（v11-luna 整段超时
// → ffmpeg exit 146），所以既要双池换节点，也要探活后再交给 ffmpeg。
//
// 即便如此，它仍只排在国内主源之后、mp3api / yuafeng 之前兜底——
// 国内源命中率高、直链稳定，优先它们更划算。
//
// 配置：
//   QISHUI_BASE=http://gomusic:8080   容器地址（默认就是 compose 里的服务名）
//   QISHUI_ENABLED=0                  整路关掉

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// qishuiShareUA 分享页必须用「桌面 Chrome」UA。
//
// 实测（东京，2026-11-23）：手机 Safari UA 下 6 次请求有 5 次分到 v11-luna
// （境外整段不通）；换成桌面 Chrome UA 后节点明显分散到 v5-se-ex-mc / v26 /
// v5-se-sjy-daily / v11，可达率从 1/6 升到 3/6。抓的是公开分享页，不需要登录。
const qishuiShareUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

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

// qishuiShareURL 第二取链池：直接扒网页版分享页，不经过 gomusic。
//
//	GET https://music.douyin.com/qishui/share/track?track_id=<id>
//
// 页面里有一段 `window._ROUTER_DATA = {...}`，结构是
// loaderData.track_page.audioWithLyricsOption，其中的 url 就是明文直链
// （mp4 容器 / AAC 音频，实测 129kbps，普通 <audio> 与 ffmpeg 都能直接吃）。
//
// 这条路径和 qishuiTrackURL 走的是完全不同的接口，CDN 节点池也不重叠：
//   - gomusic 池：v3-luna / v5-ex-luna（mp3，约 257kbps）
//   - 分享页池：v11-luna / v26-luna / v5-se-*-luna（m4a，约 130kbps）
//
// 所以它不是替代品，而是「A 池落不通节点时换 B 池」的兜底。
//
// 注意内容长度：付费曲两个池都只给 60 秒试听片段（group_playable_level=pay，
// audioWithLyricsOption.previewStart/previewEnd 标出区间，例如 149.2s→206.6s）；
// 免费曲给全长。这是上游的版权策略，换路径解决不了。
func qishuiShareURL(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("track id 为空")
	}
	raw := "https://music.douyin.com/qishui/share/track?track_id=" + url.QueryEscape(id)
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", qishuiShareUA)
	req.Header.Set("Referer", "https://music.douyin.com/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	resp, err := qishuiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("分享页 HTTP %d", resp.StatusCode)
	}
	// 分享页约 180KB，给 4MB 上限足够且不会被异常响应拖死
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}

	// _ROUTER_DATA 后面紧跟的就是一段完整 JSON，用 Decoder 读第一个对象即可
	// （其后还有 </script> 等非 JSON 内容，不能用 json.Unmarshal 整段解析）。
	idx := bytes.Index(body, []byte("_ROUTER_DATA"))
	if idx < 0 {
		return "", fmt.Errorf("分享页里没有 _ROUTER_DATA（页面结构可能变了）")
	}
	rest := body[idx:]
	brace := bytes.IndexByte(rest, '{')
	if brace < 0 {
		return "", fmt.Errorf("_ROUTER_DATA 后没有 JSON 起始符")
	}

	var data struct {
		LoaderData struct {
			TrackPage struct {
				Audio struct {
					URL      string  `json:"url"`
					Level    string  `json:"group_playable_level"`
					Duration float64 `json:"duration"`
				} `json:"audioWithLyricsOption"`
			} `json:"track_page"`
		} `json:"loaderData"`
	}
	if err := json.NewDecoder(bytes.NewReader(rest[brace:])).Decode(&data); err != nil {
		return "", fmt.Errorf("解析 _ROUTER_DATA 失败: %w", err)
	}
	link := strings.TrimSpace(data.LoaderData.TrackPage.Audio.URL)
	if link == "" {
		return "", fmt.Errorf("分享页直链为空（可能是地区限制或已下架）")
	}
	return link, nil
}

// qishuiProbeURL 用 2 字节 Range 探活，判断这个 CDN 节点到底通不通。
//
// 起因：境外命中 v11-luna 时是「TCP 连得上但整段无响应」，ffmpeg 要等十几秒
// 才 exit 146，一次失败的代价太高。这里 3 秒内就能下结论，然后直接换候选。
// 会跟随 302（v5-se-sjy-daily 这类节点先跳到 qrstuvwxyzab 再出流）。
func qishuiProbeURL(link string) bool {
	if strings.TrimSpace(link) == "" {
		return false
	}
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", qishuiShareUA)
	req.Header.Set("Range", "bytes=0-1")
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
}

// qishuiPickURL 从两个独立取链池里挑一条「探活通过」的直链，返回给转码用。
//
// 顺序：先 gomusic 池（mp3 257kbps，音质更好），落了不通节点再换分享页池，
// 分享页最多换 3 次节点（单次约 50% 可达，3 次全挂的概率约 12%）。
// 全部候选都不通时返回第一条非空链接——让 ffmpeg 去试，比直接报错更接近
// 原来的行为（有些节点探测失败但能出流）。
//
// 整个挑选过程有 12 秒预算上限，命中正常节点时只多花一次探活（约 0.3s）。
func qishuiPickURL(id string) string {
	var fallback string
	deadline := time.Now().Add(12 * time.Second)

	// 池 A：gomusic（音质优先）
	if link, err := qishuiTrackURL(id); err != nil {
		fmtLogf("[qishui] 池A 取链失败: %v", err)
	} else if link != "" {
		fallback = link
		if qishuiProbeURL(link) {
			return link
		}
		fmtLogf("[qishui] 池A 直链探测不通（命中不可达节点），改用分享页池")
	}

	// 池 B：分享页，每次请求都会重新分配节点
	const shareTries = 3
	for i := 1; i <= shareTries && time.Now().Before(deadline); i++ {
		link, err := qishuiShareURL(id)
		if err != nil || link == "" {
			if err != nil {
				fmtLogf("[qishui] 分享页取链失败（第 %d 次）: %v", i, err)
			}
			continue
		}
		if fallback == "" {
			fallback = link
		}
		if qishuiProbeURL(link) {
			fmtLogf("[qishui] 分享页池命中可达节点（第 %d 次）", i)
			return link
		}
	}
	return fallback
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
	link := qishuiPickURL(pick.ID)
	if link == "" {
		fmtLogf("[qishui] 取链失败 %s(%s)：两个取链池都没有可用直链", pick.Name, pick.ID)
		return pick, ""
	}
	return pick, link
}

// qishuiRefetchURL 在转码失败后重新取一次直链。
//
// 汽水的直链由 CDN 就近调度，每次请求可能落到不同节点。境外实测：
// gomusic 池的 v3 / v5-ex / v26-luna 都可达，v11-luna 从东京整段超时；
// 分享页池恰好相反，v11 占多数、v5-se / v26 可达。命中不通节点时 ffmpeg
// 直接 exit 146，所以这里重走 qishuiResolve —— 它内部会先探活再换池，
// 通常第一次就能给到可达链接，不必再让 ffmpeg 白跑一轮。
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
