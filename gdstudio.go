package main

// GD Studio 公益音乐 API 音源适配
// 接口文档: https://music-api.gdstudio.xyz/api.php
// 支持 types=search / url / pic / lyric
// 实测可用 source: netease（华语翻唱多、原唱常缺）、joox（中文原唱覆盖较好）
// 不可用: kuwo / tencent / migu / kugou（返回 source is not supported）

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const gdStudioAPI = "https://music-api.gdstudio.xyz/api.php"

// 音源优先级：joox 的华语原唱比例明显高于 netease，且翻唱少
var gdSourceOrder = []string{"joox", "netease"}

type gdTrack struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Artist  []string `json:"artist"`
	Album   string   `json:"album"`
	PicID   string   `json:"pic_id"`
	URLID   string   `json:"url_id"`
	LyricID string   `json:"lyric_id"`
	Source  string   `json:"source"`
	score   int      // 内部排序用，不参与 JSON

	// artistMatched 歌手是否真的对得上。只有它为 true 的候选才允许被选中：
	// 避免「点 Beyond《长城》、放成 Bobby Cole《长城》」这类同名不同人的事故。
	artistMatched bool
}

// gdFatalMarkers 命中即整条丢弃的版本标记。
// 这些版本即便时长够长也不是原唱，留在候选里只会靠「够长」抢走名额
// （典型：东风破 480s 慢四版、双截棍 CCTV 剪辑版、晴天 112s 片段）。
// 注意「原唱」不能放进来：境外曲库常用「晴天 (原唱 周杰伦)」这种标题标注原唱。
var gdFatalMarkers = []string{
	"慢四", "串烧", "合集", "伴奏", "伴唱", "纯音乐", "instrumental", "karaoke",
	"dj版", "dj版", "remix", "抖音", "快手", "铃声", "片段", "试听", "剪辑",
	"cctv", "电台", "喊麦", "蹦迪", "慢摇", "钢琴版", "小提琴", "古筝版",
	"现场", "演唱会", "(live", " live",
}

// gdFatalArtists 歌手名命中即剔除：DJ 混音号、翻唱号冒充原唱
var gdFatalArtists = []string{"dj", "翻唱", "cover", "karaoke", "伴奏", "电音"}

// 音源健康度：某个源连续取链失败就进冷却期，后面十几分钟内不再白等它。
// 境外 joox 现在就是「搜索正常、取链全空」，不冷却的话每次点歌都要
// 为它多花 2~3 秒才轮到 netease。
var (
	gdSrcMu     sync.Mutex
	gdSrcFails  = map[string]int{}
	gdSrcMuteAt = map[string]time.Time{}
)

const gdSrcCoolDown = 15 * time.Minute

func gdSrcAvailable(src string) bool {
	gdSrcMu.Lock()
	defer gdSrcMu.Unlock()
	if until, ok := gdSrcMuteAt[src]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

func gdSrcMarkFail(src string) {
	gdSrcMu.Lock()
	defer gdSrcMu.Unlock()
	gdSrcFails[src]++
	if gdSrcFails[src] >= 2 {
		gdSrcMuteAt[src] = time.Now().Add(gdSrcCoolDown)
		gdSrcFails[src] = 0
		fmt.Printf("[GDStudio] 源 %s 取链连续失败，冷却 %v\n", src, gdSrcCoolDown)
	}
}

func gdSrcMarkOK(src string) {
	gdSrcMu.Lock()
	defer gdSrcMu.Unlock()
	gdSrcFails[src] = 0
	delete(gdSrcMuteAt, src)
}

// gdActiveSources 返回当前非冷却状态的音源；全部冷却时返回完整列表（宁可慢也别无歌可放）
func gdActiveSources() []string {
	var out []string
	for _, s := range gdSourceOrder {
		if gdSrcAvailable(s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return gdSourceOrder
	}
	return out
}

// gdIsFatalTrack 判断是否属于必须剔除的版本
func gdIsFatalTrack(name string, artists []string) bool {
	l := strings.ToLower(name)
	for _, m := range gdFatalMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	a := strings.ToLower(strings.Join(artists, "/"))
	for _, m := range gdFatalArtists {
		if strings.Contains(a, m) {
			return true
		}
	}
	return false
}

type gdURLResp struct {
	URL  string `json:"url"`
	BR   int    `json:"br"`
	Size int    `json:"size"`
}

var gdHttpClient = &http.Client{Timeout: 8 * time.Second}

func gdFetch(params url.Values) ([]byte, error) {
	// gdstudio 有并发限流：同时打 4 个请求会返回 503。单请求重试一次即可恢复
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(600 * time.Millisecond)
		}
		req, err := http.NewRequest("GET", gdStudioAPI+"?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
		req.Header.Set("Accept", "application/json")
		resp, err := gdHttpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil {
			return body, nil
		}
		if readErr != nil {
			lastErr = readErr
		} else {
			lastErr = fmt.Errorf("http status %d", resp.StatusCode)
		}
	}
	return nil, lastErr
}

// ---------- 选曲打分 ----------

// 翻唱/二创的典型标记。命中即重罚，除非用户搜索词本身包含该词
var coverMarkers = []string{
	"cover", "翻唱", "翻自", "伴奏", "伴唱", "karaoke", "instrumental",
	"remix", "dj版", "女声", "男声", "童声", "童音", "钢琴", "吉他", "古筝",
	"纯音乐", "慢速", "加速", "降调", "升调", "铃声", "铃声版",
	"live", "现场", "演唱会", "0.8x", "1.2x", "片段", "试听",
	"治愈", "伤感", "烟嗓", "深情", "甜妹", "慢摇", "蹦迪", "喊麦",
	"抖音", "快手", "热播", "热搜", "神曲", "魔改", "串烧", "合集",
}

// 歌名是否带括号后缀。原版歌曲名通常不带；带括号的多是二创/版本
func gdHasBracket(s string) bool {
	return strings.ContainsAny(s, "(（[【")
}

// gdBracketText 取名称里第一个括号的内容，用于识别「(原唱 周杰伦)」这类标注。
func gdBracketText(s string) string {
	for _, pair := range [][2]string{{"(", ")"}, {"（", "）"}, {"[", "]"}, {"【", "】"}} {
		i := strings.Index(s, pair[0])
		if i < 0 {
			continue
		}
		rest := s[i+len(pair[0]):]
		j := strings.Index(rest, pair[1])
		if j < 0 {
			return rest
		}
		return rest[:j]
	}
	return ""
}


// 归一化：去括号内容、去空格、转小写
func gdNorm(s string) string {
	s = strings.TrimSpace(s)
	// 去掉中英文括号及其内容
	for _, pair := range [][2]string{{"(", ")"}, {"（", "）"}, {"[", "]"}, {"【", "】"}} {
		for {
			i := strings.Index(s, pair[0])
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], pair[1])
			if j < 0 {
				s = s[:i]
				break
			}
			s = s[:i] + s[i+j+len(pair[1]):]
		}
	}
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

// 字符重合率：want 里有多少字符出现在 got 里。
// 用来兜住繁简差异（周杰伦 / 周杰倫 重合 1/3，而 RyaVocal 重合 0）
func gdOverlap(want, got string) float64 {
	if want == "" {
		return 0
	}
	// 拉丁字母名字按「整词」比较。原来按字符集合比较，会让
	// "Beyond" 与 "Bobby Cole" 因为共享 b/o/y/e 就拿到 0.67 的高分，
	// 于是把别人的同名曲当成了原唱（长城 → Bobby Cole，实测事故）。
	if gdIsLatin(want) && gdIsLatin(got) {
		wt := gdWords(want)
		gt := gdWords(got)
		if len(wt) == 0 || len(gt) == 0 {
			return 0
		}
		hits := 0
		for _, w := range wt {
			for _, g := range gt {
				if w == g {
					hits++
					break
				}
			}
		}
		return float64(hits) / float64(len(wt))
	}
	gr := []rune(got)
	hits := 0
	total := 0
	for _, w := range []rune(want) {
		total++
		for _, g := range gr {
			if w == g {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(total)
}

// gdIsLatin 判断是否为纯拉丁字母（含数字/空格/常见分隔符）的名字。
// 中文、日文、韩文名字不走整词比较，仍用字符重合率兜繁简差异。
func gdIsLatin(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ' || r == '-' || r == '.' || r == '&' || r == '\'':
		default:
			return false
		}
	}
	return true
}

// gdWords 切词并归一（小写、去掉空段），用于拉丁名字的整词比较。
func gdWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

func gdScoreTrack(t *gdTrack, song, singer string) int {
	score := 0
	wantSong := gdNorm(song)
	wantSinger := gdNorm(singer)
	gotName := gdNorm(t.Name)
	gotArtist := ""
	if len(t.Artist) > 0 {
		gotArtist = gdNorm(strings.Join(t.Artist, "/"))
	}

	// 1) 歌名匹配度
	switch {
	case gotName == wantSong:
		score += 100 // 完全一致，通常是原版
	case strings.HasPrefix(gotName, wantSong):
		score += 45 // "晴天xxx" 这种带后缀的
	case strings.Contains(gotName, wantSong):
		score += 25
	case strings.Contains(wantSong, gotName) && gotName != "":
		score += 10
	}

	// 2) 歌名里的翻唱标记惩罚（用户自己搜的词不罚）
	rawLower := strings.ToLower(t.Name)
	for _, m := range coverMarkers {
		if strings.Contains(rawLower, m) && !strings.Contains(strings.ToLower(song), m) {
			score -= 85
			break
		}
	}

	// 2b) 带括号后缀的重罚：原版通常就叫「晴天」，而「晴天(治愈版)」是二创。
	// 这一步是关键——gdNorm() 会把括号内容删掉，光靠名字归一化区分不出来。
	// 例外：「(原唱 周杰伦)」「(原版)」这类括号是在说明原唱身份，不能当二创罚。
	// 境外曲库（joox 挂了之后只剩 netease 搬运）全靠这个标注区分完整版。
	legitBracket := strings.Contains(t.Name, "原唱") ||
		strings.Contains(t.Name, "原版") ||
		strings.Contains(t.Name, "官方")
	if gdHasBracket(t.Name) && !gdHasBracket(song) && !legitBracket {
		score -= 60
	}

	// 2c) 「(原唱 周杰伦)」这类标注：括号里点名了目标歌手，说明是照原版重录的，
	// 比随便一个翻唱号（JustForFun、阿杰之类）更接近原唱。
	// 境外曲库只剩 netease 搬运，靠这条挑版本。
	bracketNamesSinger := false
	if legitBracket && wantSinger != "" {
		bt := gdNorm(gdBracketText(t.Name))
		if bt != "" && (strings.Contains(bt, wantSinger) || gdOverlap(wantSinger, bt) >= 0.99) {
			score += 55
			bracketNamesSinger = true
		}
	}

	// 3) 歌手匹配度
	artistMatched := false
	if wantSinger != "" && gotArtist != "" {
		switch {
		case gotArtist == wantSinger:
			score += 60
			artistMatched = true
		case strings.Contains(gotArtist, wantSinger) || strings.Contains(wantSinger, gotArtist):
			score += 50
			artistMatched = true
		default:
			ov := gdOverlap(wantSinger, gotArtist)
			if ov >= 0.5 {
				score += 40
				artistMatched = true
			} else if ov > 0 {
				score += int(ov * 30)
			}
		}
	} else if wantSinger == "" {
		artistMatched = true
	}

	// 3b) 歌手完全不搭、标题也没标「原唱」→ 基本是翻唱号，压一档。
	// 不压的话《晴天》会被 JustForFun 这类翻唱号凭「完整时长」抢走。
	if !artistMatched && !legitBracket && wantSinger != "" {
		score -= 30
	}

	// 记录歌手是否对得上：只有 true 的候选才可能被选中。
	// 像「点 Beyond《长城》」这种情况，曲库里只有 Bobby Cole 的同名曲，
	// 与其硬塞别人的歌，不如让整源落空、交给后面的 mp3api（YouTube）兜底。
	t.artistMatched = artistMatched || bracketNamesSinger

	// 4) 专辑是原版专辑的加分（晴天 → 叶惠美）
	if t.Album != "" && gotName == wantSong && !strings.Contains(strings.ToLower(t.Album), "翻唱") {
		score += 5
	}

	return score
}

// gdRankTracks 按贴合度排序，返回分数 >= minScore 的候选。
// 指定了歌手时，只保留歌手真的对得上的候选：宁可这一源空手而归，
// 也不能把同名的别人的歌放给用户听。
func gdRankTracks(tracks []gdTrack, song, singer string, minScore int) []gdTrack {
	out := make([]gdTrack, 0, len(tracks))
	requireArtist := strings.TrimSpace(singer) != ""
	for _, t := range tracks {
		t.score = gdScoreTrack(&t, song, singer)
		if t.score < minScore {
			continue
		}
		if requireArtist && !t.artistMatched {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// ---------- 资源解析 ----------

// gdResolveAudioURL 用 track 的 url_id 换取可播放的直链。
// brs 按优先级给码率档：320 最均衡；ESP32 端最终转 24kHz 单声道，再高没意义。
// 同时按「文件大小 / 码率」估算时长返回（GD Studio 不提供 duration 字段）。
func gdResolveAudioURL(t *gdTrack, brs []string) (string, int) {
	id := t.URLID
	if id == "" {
		id = t.ID
	}
	for _, br := range brs {
		p := url.Values{}
		p.Set("types", "url")
		p.Set("source", t.Source)
		p.Set("id", id)
		p.Set("br", br)
		body, err := gdFetch(p)
		if err != nil {
			fmt.Printf("[GDStudio] url(br=%s) error: %v\n", br, err)
			continue
		}
		var r gdURLResp
		if err := json.Unmarshal(body, &r); err != nil {
			continue
		}
		if strings.HasPrefix(r.URL, "http") {
			dur := 0
			if r.BR > 0 && r.Size > 0 {
				dur = r.Size * 8 / (r.BR * 1000) // 字节→秒
			}
			fmt.Printf("[GDStudio] resolved %s - %s @%dkbps (%dMB, %ds)\n",
				t.Name, strings.Join(t.Artist, "/"), r.BR, r.Size/1024/1024, dur)
			return r.URL, dur
		}
	}
	return "", 0
}

// gdResolveLyric 取回 LRC 文本
func gdResolveLyric(t *gdTrack) string {
	id := t.LyricID
	if id == "" {
		id = t.ID
	}
	p := url.Values{}
	p.Set("types", "lyric")
	p.Set("source", t.Source)
	p.Set("id", id)
	body, err := gdFetch(p)
	if err != nil {
		return ""
	}
	var r struct {
		Lyric string `json:"lyric"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	return r.Lyric
}

// gdResolveCover 取回封面直链
func gdResolveCover(t *gdTrack) string {
	if t.PicID == "" {
		return ""
	}
	p := url.Values{}
	p.Set("types", "pic")
	p.Set("source", t.Source)
	p.Set("id", t.PicID)
	p.Set("size", "300")
	body, err := gdFetch(p)
	if err != nil {
		return ""
	}
	var r struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	return r.URL
}

// ---------- 对外入口 ----------

type gdCand struct {
	track    gdTrack
	url      string
	duration int
}

// gdResolveBest 按分数顺序**串行**取直链，并做「片段过滤」。
//
// 早期版本是并发 2 个求快，但实测 GD Studio 对并发很不友好：joox 的高分候选
// （周杰倫《晴天》269s 原唱）经常在并发时失败，反而让 netease 的 105 分片段版
// （112s 的 "Jay"）顶上。串行慢一点但能稳定拿到高分候选。
//
// 另外加一道保险：同一批候选里，时长明显短于最长版本（<60%）的视为片段/试听，
// 有别的选择就丢弃。这样「晴天 269s」不会被「晴天 112s」抢走。
func gdResolveBest(queue []gdTrack, maxHits int, song string) *gdCand {
	if len(queue) == 0 {
		return nil
	}

	// 逐个换直链，直到凑够 maxHits 个可播候选，或试满 12 个 / 超时 14 秒。
	// 音源要有「失败熔断」：某个源连续 2 次取不到链接（境外 joox 现在就是全挂），
	// 就别再浪费请求去试它的其它候选了。
	var got []gdCand
	fails := map[string]int{}
	dead := map[string]bool{}
	attempts := 0
	deadline := time.Now().Add(14 * time.Second)

	// 冷却中的源先剔掉（但如果全被剔光，就按原队列硬试）
	var live []gdTrack
	for _, t := range queue {
		if gdSrcAvailable(t.Source) {
			live = append(live, t)
		}
	}
	if len(live) > 0 {
		queue = live
	}

	for _, t := range queue {
		if len(got) >= maxHits || attempts >= 12 || time.Now().After(deadline) {
			break
		}
		if dead[t.Source] {
			continue
		}
		attempts++
		u, d := gdResolveAudioURL(&t, []string{"320", "128"})
		if u == "" {
			gdSrcMarkFail(t.Source)
			fails[t.Source]++
			if fails[t.Source] >= 2 {
				dead[t.Source] = true
				fmt.Printf("[GDStudio] 源 %s 连续 %d 次取链失败，本次跳过其余候选\n", t.Source, fails[t.Source])
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		gdSrcMarkOK(t.Source)
		fails[t.Source] = 0
		got = append(got, gdCand{track: t, url: u, duration: d})
	}

	if len(got) == 0 {
		return nil
	}

	// 第一道：严格同歌名。之前只按「最长」选，结果《夜曲》挑中了同场的
	// 《布拉格广场》(294s) —— 名字都不对，时长再长也没意义。
	wantNorm := gdNorm(song)
	var sameTitle []gdCand
	for _, c := range got {
		if gdNorm(c.track.Name) == wantNorm {
			sameTitle = append(sameTitle, c)
		}
	}
	pool := sameTitle
	if len(pool) == 0 {
		// 没有完全同名的（生僻写法/繁体差异），退回全部候选按分数选
		pool = got
		fmt.Printf("[GDStudio] 无同名候选，回退全量择优（%d 个）\n", len(got))
	}

	// 第二道：片段过滤。短于同批最长 75% 的视为试听/截断（晴天 112s 在这里出局）
	maxDur := 0
	for _, c := range pool {
		if c.duration > maxDur {
			maxDur = c.duration
		}
	}
	var cands []gdCand
	for _, c := range pool {
		if len(pool) > 1 && maxDur > 0 && c.duration > 0 && c.duration*4 < maxDur*3 {
			fmt.Printf("[GDStudio] 丢弃疑似片段 %q - %s (%ds，同批最长 %ds)\n",
				c.track.Name, strings.Join(c.track.Artist, "/"), c.duration, maxDur)
			continue
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		cands = pool
	}

	// 第三道：歌手匹配 > 时长完整度 > 分数。
	// 时长按批内归一化成 0~20 的加分：既偏向完整版，又不会让慢四/串烧
	// （已被 gdFatalMarkers 剔除）或错歌名（上面已拦住）凭「最长」胜出。
	minDur := maxDur
	for _, c := range cands {
		if c.duration > 0 && c.duration < minDur {
			minDur = c.duration
		}
	}
	best := &cands[0]
	bestAdj := -1 << 30
	for i := range cands {
		c := &cands[i]
		adj := c.track.score
		if maxDur > minDur && c.duration > 0 {
			adj += (c.duration - minDur) * 20 / (maxDur - minDur)
		}
		if adj > bestAdj {
			bestAdj = adj
			best = c
		}
	}
	if len(cands) > 1 {
		ds := make([]int, 0, len(cands))
		for _, c := range cands {
			ds = append(ds, c.duration)
		}
		fmt.Printf("[GDStudio] 择优 %q - %s (%ds, score=%d, 候选时长=%v)\n",
			best.track.Name, strings.Join(best.track.Artist, "/"), best.duration, best.track.score, ds)
	}
	return best
}

// GDStudioSearch 搜索并返回可直接播放的 MusicItem
func GDStudioSearch(song, singer string) MusicItem {
	keyword := strings.TrimSpace(song)
	if keyword == "" {
		return MusicItem{}
	}
	if singer != "" {
		keyword = keyword + " " + strings.TrimSpace(singer)
	}

	// 1) 各音源各搜一遍，合并候选池后统一打分
	// 冷却中的源直接跳过；全部都在冷却时仍然全试一遍。
	srcs := gdActiveSources()
	var pool []gdTrack
	for _, src := range srcs {
		p := url.Values{}
		p.Set("types", "search")
		p.Set("source", src)
		p.Set("name", keyword)
		p.Set("count", "15")
		p.Set("pages", "1")

		body, err := gdFetch(p)
		if err != nil {
			fmt.Printf("[GDStudio] search(%s) error: %v\n", src, err)
			continue
		}
		var tracks []gdTrack
		if err := json.Unmarshal(body, &tracks); err != nil {
			fmt.Printf("[GDStudio] search(%s) parse error: %v\n", src, err)
			continue
		}
		for i := range tracks {
			tracks[i].Source = src
		}
		fmt.Printf("[GDStudio] search(%s) %q -> %d hits\n", src, keyword, len(tracks))
		pool = append(pool, tracks...)
	}

	if len(pool) == 0 {
		fmt.Printf("[GDStudio] no result for %q\n", keyword)
		return MusicItem{}
	}

	// 1b) 先剔掉慢四/串烧/伴奏/剪辑这类「够长但不对」的版本，别让它们参与择优
	var kept []gdTrack
	dropped := 0
	for _, t := range pool {
		if gdIsFatalTrack(t.Name, t.Artist) {
			dropped++
			continue
		}
		kept = append(kept, t)
	}
	if dropped > 0 {
		fmt.Printf("[GDStudio] 剔除 %d 个版本类候选（慢四/串烧/伴奏/剪辑等），剩 %d 个\n", dropped, len(kept))
	}
	if len(kept) > 0 {
		pool = kept
	}

	// 2) 按音源分组排序，再交错拼接。
	// 不能混池取前 N：某个源的同名版本会霸占名额（夜曲在 joox 有 3 个同分版本，
	// 且都取不到链接），交错才能保证另一个源有机会顶上。
	var queue []gdTrack
	bySrc := map[string][]gdTrack{}
	for _, t := range pool {
		bySrc[t.Source] = append(bySrc[t.Source], t)
	}
	for _, src := range gdSourceOrder {
		ranked := gdRankTracks(bySrc[src], song, singer, -30)
		bySrc[src] = ranked
	}
	for round := 0; round < 10; round++ {
		for _, src := range gdSourceOrder {
			if round < len(bySrc[src]) {
				queue = append(queue, bySrc[src][round])
			}
		}
	}
	// 全部被筛掉时：没指定歌手才退回原始顺序碰运气；
	// 指定了歌手就说明这源确实没有原唱，直接放弃，别再拿同名曲凑数。
	if len(queue) == 0 {
		if strings.TrimSpace(singer) != "" {
			fmt.Printf("[GDStudio] 无歌手可信候选，跳过本源: %q - %q\n", song, singer)
			return MusicItem{}
		}
		queue = pool
	}

	// 3) 按分数逐个换直链
	if len(queue) > 0 {
		n := len(queue)
		if n > 6 {
			n = 6
		}
		for i := 0; i < n; i++ {
			fmt.Printf("[GDStudio] cand #%d score=%d %q - %q (%s)\n",
				i+1, queue[i].score, queue[i].Name, strings.Join(queue[i].Artist, "/"), queue[i].Source)
		}
	}
	best := gdResolveBest(queue, 5, song)
	if best == nil {
		fmt.Printf("[GDStudio] no playable source for %q\n", keyword)
		return MusicItem{}
	}
	t := best.track
	item := MusicItem{
		Title:        t.Name,
		Artist:       strings.Join(t.Artist, "/"),
		AudioURL:     best.url,
		AudioFullURL: best.url,
		CoverURL:     gdResolveCover(&t),
		Duration:     best.duration,
	}
	fmt.Printf("[GDStudio] HIT %q by %q from %s (score=%d)\n",
		item.Title, item.Artist, t.Source, t.score)
	return item
}
