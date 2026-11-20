package main

// 酷我「波点」通道：按「歌名 + 歌手」直接搜索并取完整音源。
//
// 为什么需要它：网易云把周杰伦等一批歌手的作品整批下架，搜索/歌单/歌手页
// 都拿不到 song id，解灰再强也无从下手。酷我有这批版权，且波点通道返回的是
// 完整 320k 音源（不是 11 秒试听），因此可以完全绕开网易云。
//
// 流程：search.kuwo.cn 搜索 -> 按歌名/歌手打分选曲 -> bd-api.kuwo.cn 取直链。

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// kwBlocked 表示酷我通道当前不可用（通常是服务器在境外，取链返回 407 地域限制）。
// 置位后本进程内直接跳过酷我，避免每首歌都白等 5-10 秒超时。
// 也可以用环境变量 MUSIC_SOURCE_MODE=overseas 在启动时强制跳过。
var kwBlocked atomic.Bool

func init() {
	if strings.EqualFold(os.Getenv("MUSIC_SOURCE_MODE"), "overseas") {
		kwBlocked.Store(true)
		fmt.Println("[Kuwo] MUSIC_SOURCE_MODE=overseas, 跳过酷我通道")
	}
}

// kwIsRegionBlock 判断是不是地域限制类错误码
func kwIsRegionBlock(code int) bool {
	return code == 407 || code == 433
}

type kwSong struct {
	RID      string
	Name     string
	Artist   string
	Album    string
	Duration int
	score    int
	fatal    bool // 明显不是原唱的版本（伴奏/消音/cos/抖音版…）
	noVocal  bool // 纯器乐（伴奏/消音/纯音乐），连人声都没有，不能当翻唱听
}

var kwClient = &http.Client{Timeout: 9 * time.Second}

// 搜索结果是 kuwo 自家风格的结构，字段全大写且 DURATION 是字符串
type kwSearchResp struct {
	Content []struct {
		MusicPage struct {
			AbsList []struct {
				MusicRID string `json:"MUSICRID"`
				SongName string `json:"SONGNAME"`
				Artist   string `json:"ARTIST"`
				Album    string `json:"ALBUM"`
				Duration string `json:"DURATION"`
			} `json:"abslist"`
		} `json:"musicpage"`
	} `json:"content"`
}

func kwRequestWithUA(rawURL, method, ua string, headers map[string]string, body string) ([]byte, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	if ua == "" {
		ua = "okhttp/3.10.0"
	}
	req.Header.Set("User-Agent", ua)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := kwClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func kwRequest(rawURL, method string, headers map[string]string, body string) ([]byte, error) {
	return kwRequestWithUA(rawURL, method, "", headers, body)
}

// kwSearch 用 UNM 同款参数搜索，结果比 web_2013 那套干净得多
func kwSearch(song, singer string) []kwSong {
	keyword := strings.TrimSpace(song + " " + singer)
	api := "http://search.kuwo.cn/r.s?&correct=1&vipver=1&stype=comprehensive" +
		"&encoding=utf8&rformat=json&mobi=1&show_copyright_off=1&searchapi=6&all=" +
		url.QueryEscape(keyword)

	body, err := kwRequest(api, "GET", nil, "")
	if err != nil {
		fmt.Printf("[Kuwo] search error: %v\n", err)
		return nil
	}
	var resp kwSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[Kuwo] search json error: %v\n", err)
		return nil
	}

	var out []kwSong
	for _, c := range resp.Content {
		for _, it := range c.MusicPage.AbsList {
			rid := it.MusicRID
			if i := strings.LastIndex(rid, "_"); i >= 0 {
				rid = rid[i+1:]
			}
			d, _ := strconv.Atoi(it.Duration)
			out = append(out, kwSong{
				RID:      rid,
				Name:     strings.ReplaceAll(it.SongName, "&nbsp;", " "),
				Artist:   strings.ReplaceAll(it.Artist, "&nbsp;", " "),
				Album:    strings.ReplaceAll(it.Album, "&nbsp;", " "),
				Duration: d,
			})
		}
	}
	return out
}

// kwFatalArtistMarkers 歌手字段里出现这些词，说明这条是翻唱/搬运的拼接条目，
// 即便歌名歌手都对得上也不能用。
//
// 实测踩坑：《理想三旬》− 陈鸿宇，酷我有一条把歌手写成
// 「陈鸿宇&翻唱者：王煜航」、时长只有 130 秒（正片 210 秒）的条目。
// 歌手字段「包含陈鸿宇」所以 kwArtistMatch 判定通过，得分 160 排到第一，
// 结果设备放出来是半首翻唱。这类信息是上传者自己塞的，只能按关键词挡。
var kwFatalArtistMarkers = []string{
	"翻唱", "cover", "karaoke", "伴奏", "dj", "电音", "remix", "改编", "合辑", "串烧",
}

// 纯器乐标记：这些版本没有人声，不能当「翻唱」用
var kwInstrumentalWords = []string{
	"伴奏", "伴唱", "消音", "纯音乐", "ktv", "karaoke", "instrumental",
}

// 酷我上同一首歌常有 伴奏 / Live / 纯音乐 / DJ / 抖音版，必须罚掉
var kwBadWords = []string{
	"伴奏", "伴唱", "消音", "纯音乐", "ktv", "karaoke", "instrumental",
	"live", "现场", "演唱会", "cover", "翻唱", "翻自", "原唱:", "原唱：",
	"dj", "铃声", "片段", "试听", "升调", "降调", "加速", "慢速",
	"女声", "男声", "童声", "女版", "男版", "钢琴", "吉他", "治愈", "阿卡贝拉",
	"合辑", "串烧", "remix", "混音", "改编", "复刻", "高仿", "车载", "慢摇",
	"抖音", "热播", "热歌", "网红", "cos", "0.8x", "1.2x", "8d", "环绕",
	"慢四", "慢三", "快四", "舞曲", "广场舞", "交谊舞", "探戈", "伦巴", "迪斯科", "电音",
	"cctv", "音乐频道", "电台", "春晚", "综艺", "音综", "节目", "纯享", "饭制", "剪辑",
	"超燃", "燃版", "高燃", "鬼畜", "变声", "ai翻唱",
	"小提琴", "大提琴", "二胡", "古筝", "琵琶", "萨克斯", "口琴", "手风琴",
	"架子鼓", "尤克里里", "长笛", "钢琴曲", "八音盒", "陶笛", "葫芦丝",
}

// kwNormName 在 gdNorm 基础上再抹平符号差异。
// 酷我常把歌名写成「热爱105°C的你」，而用户/固件给的是「热爱105度的你」，
// 不归一化就会被判成「不是同一首歌」而白白丢掉原唱。
func kwNormName(s string) string {
	n := gdNorm(s)
	for _, r := range [][2]string{
		{"°c", "度"}, {"℃", "度"}, {"°", ""},
		{"&", ""}, {"＆", ""}, {"·", ""}, {"•", ""},
		{"！", ""}, {"!", ""}, {"？", ""}, {"?", ""},
	} {
		n = strings.ReplaceAll(n, r[0], r[1])
	}
	return n
}

// kwArtistMatch 歌手名比对。酷我常写繁体/异体字（米津玄師 vs 米津玄师），
// 纯字符串比对会把这些当成「换了个人唱」，所以再用字符重合度兜底。
func kwArtistMatch(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	if strings.Contains(got, want) || strings.Contains(want, got) {
		return true
	}
	gr := []rune(got)
	wr := []rune(want)
	set := make(map[rune]bool, len(wr))
	for _, r := range wr {
		set[r] = true
	}
	hit := 0
	for _, r := range gr {
		if set[r] {
			hit++
		}
	}
	base := len(gr)
	if len(wr) < base {
		base = len(wr)
	}
	if base == 0 {
		return false
	}
	// 严格大于 0.5：两个字的名字必须全中（周敏≠周深，不能算匹配）
	return float64(hit)/float64(base) > 0.5
}

func kwScoreTrack(t *kwSong, song, singer string) (int, bool) {
	score := 0
	fatal := false
	wantName := kwNormName(song)
	gotName := kwNormName(t.Name)

	switch {
	case gotName == wantName:
		score += 100
	case strings.Contains(gotName, wantName) || strings.Contains(wantName, gotName):
		score += 55
	default:
		score -= 40
	}

	// 歌手匹配：酷我用「&」分隔多人，也常用 Jay Chou 这类英文名
	gotArtist := strings.ToLower(t.Artist)
	wantSinger := strings.ToLower(strings.TrimSpace(singer))
	if wantSinger != "" && wantSinger != "未知" {
		alts := []string{wantSinger}
		if strings.Contains(wantSinger, "周杰伦") {
			alts = append(alts, "jay chou", "jay")
		}
		hit := false
		for _, a := range alts {
			if kwArtistMatch(gotArtist, a) {
				hit = true
				break
			}
		}
		if hit {
			score += 60
			// 歌手完全一致（而不是「周杰伦&那英」这种合唱/串场版）再加一点，
			// 用来在同名版本里压过合唱歌手
			if gotArtist == wantSinger {
				score += 10
			}
		} else if gotArtist == "" {
			score -= 20 // 歌手字段为空，多半是搬运/翻唱
		} else {
			score -= 70 // 换了个歌手唱，基本可以认定不是原唱
		}
	}

	// 歌手字段被上传者塞成「陈鸿宇&翻唱者：王煜航」这种，整条判死
	lowerArtistAll := strings.ToLower(t.Artist)
	for _, m := range kwFatalArtistMarkers {
		if strings.Contains(lowerArtistAll, m) {
			// 罚到阈值以下，连第二轮「翻唱顶上」的机会都不给
			score -= 200
			fatal = true
			break
		}
	}

	// 版本噪声：既罚分，也标记 fatal，让它在第一轮筛选中被跳过
	lowerName := strings.ToLower(t.Name)
	lowerAlbum := strings.ToLower(t.Album)
	lowerSong := strings.ToLower(song)
	for _, w := range kwBadWords {
		if strings.Contains(lowerSong, w) {
			continue // 用户自己就在搜伴奏/纯音乐，不算噪声
		}
		if strings.Contains(lowerName, w) || strings.Contains(lowerAlbum, w) {
			score -= 90
			fatal = true
			for _, iw := range kwInstrumentalWords {
				if w == iw {
					t.noVocal = true
					break
				}
			}
			break
		}
	}

	// 时长合理性：1 分钟以下基本都是片段/试听
	if t.Duration > 0 && t.Duration < 60 {
		score -= 80
		fatal = true
	}
	return score, fatal
}

// kwRank 打分并按分数降序排好，返回全部候选（由调用方决定试几条）。
// 同分时优先时长更长的那条——同样都带「伴奏」标记时，102s 的明显是被截断的。
func kwRank(list []kwSong, song, singer string) []kwSong {
	for i := range list {
		list[i].score, list[i].fatal = kwScoreTrack(&list[i], song, singer)
	}

	// 同一首歌的各版本时长应该接近。明显短于「中位时长」的，基本是截断片段
	// （例：海底有个 80 秒的「正式版」，正片是 271 秒）。
	// 注意这里必须用中位数而不是最大值：搜索结果里常混进几分钟的串烧/合辑，
	// 用最大值会把正常单曲全部误判成截断（晴天就踩过这个坑）。
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
		if len(durs)%2 == 0 {
			median = (durs[len(durs)/2-1] + durs[len(durs)/2]) / 2
		}
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
		// 同分时选时长最接近中位数的：原版通常就是被发得最多的那一版。
		// 不能按「越长越好」排，否则会挑中慢四版/串烧这类加长版本
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

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// kwSign 复刻波点接口的签名：
// 取 "?" 之后全部字符、只留字母数字、逐字符排序，md5("kuwotest"+排序串+pathname)
func kwSign(raw string) string {
	u, err := url.Parse(raw)
	path := ""
	if err == nil {
		path = u.Path
	}
	raw += "&timestamp=" + strconv.FormatInt(time.Now().UnixMilli(), 10)

	q := raw
	if i := strings.Index(q, "?"); i >= 0 {
		q = q[i+1:]
	}
	var chars []rune
	for _, c := range q {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			chars = append(chars, c)
		}
	}
	sort.Slice(chars, func(i, j int) bool { return chars[i] < chars[j] })

	sum := md5.Sum([]byte("kuwotest" + string(chars) + path))
	return raw + "&sign=" + hex.EncodeToString(sum[:])
}

// 波点接口要求先「看一次广告」解锁付费歌曲，否则取链返回 20018。
// devid 必须整个进程固定（解锁状态可能按设备绑定），不能每次随机。
var kwDeviceID = strconv.FormatInt(time.Now().UnixNano()%100000000000, 10)

var kwQimei = "1e9970cbcdc20a031dee9f37100017e1840e"

func kwBodianHeaders() map[string]string {
	return map[string]string{
		"plat":            "ar",
		"channel":         "aliopen",
		"devid":           kwDeviceID,
		"ver":             "3.9.0",
		"X-Forwarded-For": "1.0.1.114",
	}
}

// kwAdFree 触发一次广告解锁，结果本身不用，失败也不影响后面对免费曲的取链。
//
// 为什么必须只调一次：解锁状态是按 devid 绑定在服务端的，一次调用长期有效。
// 早期实现是「每取一条链接就解锁一次」，等于把首字节时间平白拉长了 3~5 秒
// （实测海阔天空/起风了/生僻字 首字节 5.8~6.3s，而解锁只需一次的歌只要 0.9s）。
// 现在用 sync.Once 保证整个进程只解锁一次；只有取链返回 20018（服务端认为
// 未解锁）时才强制重解一次。
var (
	kwAdOnce   sync.Once
	kwAdMu     sync.Mutex
	kwAdLastAt time.Time
)

func kwAdFreeReal() {
	adURL := "http://bd-api.kuwo.cn/api/service/advert/watch" +
		"?uid=-1&token=&timestamp=1724306124436&sign=15a676d66285117ad714e8c8371691da"
	h := map[string]string{
		"plat":         "ar",
		"channel":      "aliopen",
		"devid":        kwDeviceID,
		"ver":          "3.9.0",
		"qimei36":      kwQimei,
		"content-type": "application/json; charset=utf-8",
	}
	body := `{"type":5,"subType":5,"musicId":0,"adToken":""}`
	if _, err := kwRequestWithUA(adURL, "POST", "Dart/2.19 (dart:io)", h, body); err != nil {
		fmt.Printf("[Kuwo] adfree error: %v\n", err)
	}
	kwAdMu.Lock()
	kwAdLastAt = time.Now()
	kwAdMu.Unlock()
}

func kwAdFree() {
	kwAdOnce.Do(kwAdFreeReal)
}

// kwAdFreeRefresh 服务端认为未解锁时重解，但 60 秒内不重复打，避免被风控
func kwAdFreeRefresh() {
	kwAdMu.Lock()
	if time.Since(kwAdLastAt) < time.Minute {
		kwAdMu.Unlock()
		return
	}
	kwAdMu.Unlock()
	kwAdFreeReal()
}

// kwBodianURL 用 rid 换完整音源的直链
func kwBodianURL(rid string) string {
	return kwBodianURLDepth(rid, 0)
}

func kwBodianURLDepth(rid string, depth int) string {
	raw := "http://bd-api.kuwo.cn/api/play/music/v2/audioUrl?&br=320kmp3&musicId=" + rid
	signed := kwSign(raw)

	kwAdFree() // 付费曲必须先解锁，否则返回 20018（进程内只解一次）
	body, err := kwRequestWithUA(signed, "GET", "Dart/2.19 (dart:io)", kwBodianHeaders(), "")
	if err != nil {
		fmt.Printf("[Kuwo] bodian error: %v\n", err)
		return ""
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			AudioURL string `json:"audioUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Printf("[Kuwo] bodian json error: %v\n", err)
		return ""
	}
	if resp.Code == 20018 && depth == 0 {
		// 服务端不认之前的解锁状态，重解一次再试
		fmt.Printf("[Kuwo] 取链返回 20018，重新解锁后重试\n")
		kwAdFreeRefresh()
		return kwBodianURLDepth(rid, 1)
	}
	if resp.Code != 200 || !strings.HasPrefix(resp.Data.AudioURL, "http") {
		// 407/433：境外 IP 被版权地域限制，立即停用酷我，别再把时间耗在重试上
		if kwIsRegionBlock(resp.Code) {
			if !kwBlocked.Swap(true) {
				fmt.Printf("[Kuwo] 地域限制(code=%d)，本进程内停用酷我，后续改走其它音源\n", resp.Code)
			}
			return ""
		}
		fmt.Printf("[Kuwo] bodian code=%d body=%.120s\n", resp.Code, string(body))
		return ""
	}
	return resp.Data.AudioURL
}

// KuwoSearch 对外入口：给歌名和歌手，返回可直接播放（完整曲目）的音源
//
// 选曲分两轮，宁可多试几条也不要把伴奏/翻唱当成原唱：
//   1) 正常候选（非 fatal 且分数够）—— 目标就是原唱
//   2) 只有翻唱/DJ/Live 这类「有人声」的版本时，允许它们顶上
// 纯器乐（伴奏、消音、纯音乐）不进任何一轮：用户要的是歌，不是伴奏带。
// 分数不够的一律不试，宁可返回空交给下一层音源，也不要放一首完全不相干的歌
// （曾经因为放宽到「凑合放」，把《后来遇见他》放成了《大梦归》）。
func KuwoSearch(song, singer string) MusicItem {
	return kuwoResolve(song, singer, false)
}

// KuwoSearchStrict 只要原唱，不接受 Live/翻唱/DJ 这类替代版本。
// 用在多源链的前段：先让各源都试一遍真原唱，都不行再用 KuwoSearch 的宽容模式。
func KuwoSearchStrict(song, singer string) MusicItem {
	return kuwoResolve(song, singer, true)
}

func kuwoResolve(song, singer string, strict bool) MusicItem {
	if strings.TrimSpace(song) == "" {
		return MusicItem{}
	}
	// 已判定地域受限（境外服务器）时直接跳过，交给后面的音源
	if kwBlocked.Load() {
		return MusicItem{}
	}
	list := kwSearch(song, singer)
	if len(list) == 0 {
		fmt.Printf("[Kuwo] no result for %q - %q\n", song, singer)
		return MusicItem{}
	}
	if len(list) > 10 {
		list = list[:10]
	}
	ranked := kwRank(list, song, singer)

	var queue []kwSong
	for _, t := range ranked {
		if !t.fatal && t.score >= 40 {
			queue = append(queue, t)
		}
	}
	if !strict {
		// 宽容模式才允许「有人声的翻唱/Live 版」顶上；纯器乐任何模式都不进
		for _, t := range ranked {
			if t.fatal && !t.noVocal && t.score >= 40 {
				queue = append(queue, t)
			}
		}
	}
	if len(queue) > 6 {
		queue = queue[:6]
	}

	for i, t := range queue {
		if i < 3 {
			fmt.Printf("[Kuwo] cand #%d score=%d fatal=%v %q - %q (%s, %ds)\n",
				i+1, t.score, t.fatal, t.Name, t.Artist, t.Album, t.Duration)
		}
		audioURL := kwBodianURL(t.RID)
		if audioURL == "" {
			continue
		}
		fmt.Printf("[Kuwo] HIT #%d %q - %q (%ds, score=%d)\n", i+1, t.Name, t.Artist, t.Duration, t.score)
		return MusicItem{
			Title:        t.Name,
			Artist:       t.Artist,
			AudioURL:     audioURL,
			AudioFullURL: audioURL,
			Duration:     t.Duration,
		}
	}
	fmt.Printf("[Kuwo] all candidates unusable for %q - %q\n", song, singer)
	return MusicItem{}
}
