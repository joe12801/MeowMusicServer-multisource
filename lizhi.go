package main

// 荔枝 FM（lizhi.fm）—— 播客 / 有声书 / 网络电台
//
// 定位：和喜马拉雅同类，**只在自身筛选按钮下出现**的补充源。
// 它的内容是有声书、播客、电台节目，和唱片公司的曲库完全不重叠
// （搜「周杰伦」返回的是《周杰伦开直播？爷青回！》这种谈话节目，不是歌），
// 所以不参与 getRemoteMusicURL 多源链路，也不进「全部」搜索。
//
// 走 H5 端公开接口（2026-11-23 在东京机器实测）：
//
//	搜索  GET https://m.lizhi.fm/vodapi/search/voice
//	      ?deviceId=<h5-fixed>&keywords=<kw>&page=<n>&receiptData=<上次的>
//	      → data[]：voiceInfo{voiceId,name,duration,imageUrl}
//	                userInfo{name,photo}（主播）
//	                voicePlayProperty{trackUrl, playAccessProperty{accessPermission}}
//	      → 顶层 receiptData 是翻页游标，不带它 page=2 会重复返回第一页
//	取链  GET https://m.lizhi.fm/vodapi/voice/info/<voiceId>
//	      → data.userVoice.voicePlayProperty.trackUrl
//
// 四个实测坑（都记在这里，别再踩一遍）：
//  1. **UA 认 iPhone**。H5 接口用 Chrome 桌面 UA 直接 403，换成 iPhone Safari
//     UA 才 200；音频 CDN 同样校验 UA（无头请求 403，带 iPhone UA 206）。
//  2. **付费内容没有 trackUrl**。会员/付费单集 `playAccessProperty.accessPermission`
//     是 4（免费是 2），`voiceExProperty.voicePrice > 0`，且 `trackUrl` 字段
//     整个缺失，只有一个 `voiceAuditionProperty: 210`（试听 210 秒）。
//     这类条目既不列也不播——按「没有播放地址」统一过滤。
//  3. **只有 `_hd.mp3` 这一档可用**。musicdl 里写的 `_ud.mp3` 现在全部 404，
//     `_sd.m4a` 能拉但码率只有约 14kbps（人声都糊），所以统一用 `_hd.mp3`
//     （实测 128kbps）。
//  4. **单集可能超长**。有声书单集从 69 秒到 3 小时都有，3 小时那档要下载
//     117MB、转码约 70 秒，逼近 ensureMusicCache 的 240s 上限，所以加了个
//     时长闸门，见 lizhiMaxDuration。

import (
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
	"unicode"
)

const (
	sourceLizhi = "lizhi"

	// 播放地址里携带的 xid 形如 lz:<voiceId>:<yyyymmdd>。
	// 必须带前缀：荔枝的 voiceId 是纯数字，和喜马拉雅的 trackId 完全同形，
	// 不带前缀就会被当成喜马拉雅的内容。
	// 末尾的日期用于「详情接口挂了」时按 CDN 路径规则拼兜底地址。
	lizhiXIDPrefix = "lz:"

	lizhiSearchAPI = "https://m.lizhi.fm/vodapi/search/voice"
	lizhiInfoAPI   = "https://m.lizhi.fm/vodapi/voice/info/"

	// 荔枝 H5 自己用的固定设备号，服务端不校验，照抄即可
	lizhiDeviceID = "h5-b6ef91a9-3dbb-c716-1fdd-43ba08851150"

	lizhiUA      = "Mozilla/5.0 (iPhone; CPU iPhone OS 9_1 like Mac OS X) " +
		"AppleWebKit/601.1.46 (KHTML, like Gecko) Version/9.0 " +
		"Mobile/13B143 Safari/601.1"
	lizhiReferer = "https://m.lizhi.fm"

	// 单集时长上限（秒）。3 小时约 173MB，转码约 100 秒，
	// 留在 ensureMusicCache 的 240s 预算内；再长就风险太大。
	lizhiMaxDuration = 3 * 3600

	// 一次搜索最多翻几页（每页约 15~18 条）
	lizhiMaxPages = 3
)

// lzTrack 是荔枝的一集节目
type lzTrack struct {
	ID       string
	Name     string
	Artist   string // 主播名
	Cover    string
	Duration int
	TrackURL string
	Day      string // yyyymmdd，用于兜底拼 CDN 路径
}

var (
	lizhiClientOnce sync.Once
	lizhiClient     *http.Client
)

func lizhiHTTPClient() *http.Client {
	lizhiClientOnce.Do(func() {
		lizhiClient = &http.Client{Timeout: 20 * time.Second}
	})
	return lizhiClient
}

// lizhiEnabled 开关：LIZHI_ENABLED=0 关掉（默认开）
func lizhiEnabled() bool {
	v := strings.TrimSpace(os.Getenv("LIZHI_ENABLED"))
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "off")
}

// lizhiGet 带 iPhone UA / Referer 请求荔枝接口。
//
// 必须显式带 UA：Chrome 桌面 UA 会被 WAF 直接 403。
func lizhiGet(rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", lizhiUA)
	req.Header.Set("Referer", lizhiReferer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	resp, err := lizhiHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return body, nil
}

// lizhiStreamHeaders 转码时交给 ffmpeg 的请求头。
// 荔枝音频 CDN 校验 UA，ffmpeg 默认的 "Lavf/x.y" 会被 403。
func lizhiStreamHeaders() map[string]string {
	return map[string]string{
		"User-Agent": lizhiUA,
		"Referer":    lizhiReferer,
	}
}

// ---------- 数据结构 ----------

type lzSearchResp struct {
	Code        int    `json:"code"`
	Msg         string `json:"msg"`
	ReceiptData string `json:"receiptData"`
	Data        []struct {
		UserInfo struct {
			Name  string `json:"name"`
			Photo string `json:"photo"`
		} `json:"userInfo"`
		VoiceInfo struct {
			VoiceID  json.Number `json:"voiceId"`
			Name     string      `json:"name"`
			Duration int         `json:"duration"`
			ImageURL string      `json:"imageUrl"`
		} `json:"voiceInfo"`
		VoiceExProperty struct {
			VoicePrice int `json:"voicePrice"`
		} `json:"voiceExProperty"`
		VoicePlayProperty struct {
			TrackURL           string `json:"trackUrl"`
			PlayAccessProperty struct {
				AccessPermission int `json:"accessPermission"`
				AccessType       int `json:"accessType"`
			} `json:"playAccessProperty"`
		} `json:"voicePlayProperty"`
	} `json:"data"`
}

type lzInfoResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		UserVoice struct {
			VoiceInfo struct {
				VoiceID  json.Number `json:"voiceId"`
				Name     string      `json:"name"`
				Duration int         `json:"duration"`
				ImageURL string      `json:"imageUrl"`
			} `json:"voiceInfo"`
			VoicePlayProperty struct {
				TrackURL           string `json:"trackUrl"`
				PlayAccessProperty struct {
					AccessPermission int `json:"accessPermission"`
				} `json:"playAccessProperty"`
			} `json:"voicePlayProperty"`
		} `json:"userVoice"`
	} `json:"data"`
}

// ---------- 工具 ----------

// lzDayFromURL 从直链/封面里抽出 yyyymmdd。
//
//	https://cdn5.lizhi.fm/audio/2020/09/01/5130..._hd.mp3   → 20200901
//	https://cdnimg101.gzlzfm.com/audio_cover/2020/10/08/x.jpg → 20201008
func lzDayFromURL(u string) string {
	if u == "" {
		return ""
	}
	parts := strings.Split(u, "/")
	for i := 0; i+2 < len(parts); i++ {
		y, m, d := parts[i], parts[i+1], parts[i+2]
		if len(y) != 4 || len(m) != 2 || len(d) != 2 {
			continue
		}
		if _, err := strconv.Atoi(y); err != nil {
			continue
		}
		if _, err := strconv.Atoi(m); err != nil {
			continue
		}
		if _, err := strconv.Atoi(d); err != nil {
			continue
		}
		return y + m + d
	}
	return ""
}

// lzHTTPS 把直链升到 https。
// 荔枝返回的 trackUrl 是 http://；这台机器在境外，走明文拉国内 CDN
// 容易被中间设备改包，换成 https 更稳（cdn5/cdn101 都支持）。
func lzHTTPS(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "http://") {
		return "https://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// lzFallbackURL 按荔枝 CDN 的固定路径规则拼直链。
// 详情接口不可用时才有用；日期取自 voiceId 对应音频的存放目录。
// 域名和主链路保持一致用 cdn5（实测比 cdn101 快：117MB 用 9.9s vs 15.0s）。
func lzFallbackURL(voiceID, day string) string {
	if len(day) != 8 || voiceID == "" {
		return ""
	}
	return fmt.Sprintf("https://cdn5.lizhi.fm/audio/%s/%s/%s/%s_hd.mp3",
		day[:4], day[4:6], day[6:8], voiceID)
}

// lizhiTrackXID 拼播放用的 xid（lz:<voiceId>:<yyyymmdd>）
func lizhiTrackXID(voiceID, day string) string {
	voiceID = strings.TrimSpace(voiceID)
	if voiceID == "" {
		return ""
	}
	return lizhiXIDPrefix + voiceID + ":" + strings.TrimSpace(day)
}

// lzParseXID 拆 xid → (voiceId, day)
func lzParseXID(xid string) (string, string) {
	xid = strings.TrimPrefix(strings.TrimSpace(xid), lizhiXIDPrefix)
	parts := strings.SplitN(xid, ":", 2)
	id := strings.TrimSpace(parts[0])
	day := ""
	if len(parts) == 2 {
		day = strings.TrimSpace(parts[1])
	}
	if len(day) != 8 {
		day = ""
	}
	return id, day
}

// lizhiBackupURLs 返回同一直链的备用线路（cdn5 ↔ cdn101）。
//
// 荔枝的音频直链不带签名、不带过期时间，两个 CDN 域名指向同一份文件，
// 所以转码失败时换个域名重试是有效的（和 B 站重签 URL 不同，这里不用重新取链）。
// 实测 cdn5 下载更快（9.9s vs 15.0s / 117MB），所以主链走 cdn5，备用给 cdn101。
func lizhiBackupURLs(u string) []string {
	u = strings.TrimSpace(u)
	if u == "" {
		return nil
	}
	out := make([]string, 0, 2)
	if strings.Contains(u, "//cdn5.lizhi.fm/") {
		out = append(out, strings.Replace(u, "//cdn5.lizhi.fm/", "//cdn101.lizhi.fm/", 1))
	}
	if strings.Contains(u, "//cdn101.lizhi.fm/") {
		out = append(out, strings.Replace(u, "//cdn101.lizhi.fm/", "//cdn5.lizhi.fm/", 1))
	}
	return out
}

// lzNormRunes 归一化：只保留字母/数字并转小写，去掉标点和空白。
//
// 用来做「相关性」比较——荔枝标题里塞满了《》、|、——、emoji，
// 不归一化的话「《明朝那些事儿》」和「明朝那些事儿」会被当成不同的串。
func lzNormRunes(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToLower(r))
		}
	}
	return out
}

// lzBigrams 抽二元组。
//
// 为什么不用「包含子串」：中文没有空格，query「明朝那些事儿」直接当子串匹配
// 会漏掉「《明朝那些事儿》1朱元璋」这种带标点的标题；而用单字匹配又会把
// 「遗憾的事」这种只共用一个「事」的无关内容放进来。二元组正好卡在中间。
func lzBigrams(s string) map[string]bool {
	rs := lzNormRunes(s)
	m := make(map[string]bool, len(rs))
	if len(rs) == 0 {
		return m
	}
	if len(rs) == 1 {
		// 单字查询退化成单字匹配，否则二元组为空会判成「全不相关」
		m[string(rs)] = true
		return m
	}
	for i := 0; i+1 < len(rs); i++ {
		m[string(rs[i:i+2])] = true
	}
	return m
}

// lzMatchQuery 判断一条结果和查询词是否沾边。
//
// 荔枝的搜索是按分词召回的，噪音不小（实测搜「明朝那些事儿」会返回一条
// 叫「遗憾的事」的 70 秒语音，搜「郭德纲 相声」会返回心理咨询讲座）。
// 这里只做「至少命中一个二元组」的宽松过滤：歌名和主播名任意一边沾边就保留，
// 因为像「罗辑思维」这种节目，标题里根本没有节目名，只有主播名。
func lzMatchQuery(grams map[string]bool, t lzTrack) bool {
	if len(grams) == 0 {
		return true
	}
	text := lzNormRunes(t.Name + " " + t.Artist)
	if len(text) == 0 {
		return false
	}
	for g := range grams {
		if strings.Contains(string(text), g) {
			return true
		}
	}
	return false
}

// ---------- 搜索 ----------

// lzSearchPage 拉一页搜索结果，返回本轮结果和下一页游标。
func lzSearchPage(keyword string, page int, receipt string) ([]lzTrack, string, error) {
	q := url.Values{}
	q.Set("deviceId", lizhiDeviceID)
	q.Set("keywords", keyword)
	q.Set("page", strconv.Itoa(page))
	q.Set("receiptData", receipt)

	body, err := lizhiGet(lizhiSearchAPI + "?" + q.Encode())
	if err != nil {
		return nil, "", err
	}
	var resp lzSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("搜索响应解析失败: %w", err)
	}
	if resp.Code != 0 {
		msg := strings.TrimSpace(resp.Msg)
		if msg == "" {
			msg = fmt.Sprintf("code=%d", resp.Code)
		}
		return nil, "", fmt.Errorf("荔枝搜索失败: %s", msg)
	}

	out := make([]lzTrack, 0, len(resp.Data))
	for _, it := range resp.Data {
		id := strings.TrimSpace(it.VoiceInfo.VoiceID.String())
		name := strings.TrimSpace(it.VoiceInfo.Name)
		perm := it.VoicePlayProperty.PlayAccessProperty.AccessPermission
		trackURL := strings.TrimSpace(it.VoicePlayProperty.TrackURL)

		if id == "" || id == "0" || name == "" {
			continue
		}
		// 付费/会员单集：accessPermission=4 且没有 trackUrl，
		// 只有 voiceAuditionProperty（试听 210 秒）。这类直接跳过，
		// 别让用户点了才发现只能听 3 分半。
		if perm != 0 && perm != 2 {
			continue
		}
		if it.VoiceExProperty.VoicePrice > 0 {
			continue
		}
		if trackURL == "" {
			continue
		}
		// 超长单集（>3h）转码会顶到 240s 超时，宁可不列
		if it.VoiceInfo.Duration > lizhiMaxDuration {
			continue
		}

		day := lzDayFromURL(trackURL)
		if day == "" {
			day = lzDayFromURL(it.VoiceInfo.ImageURL)
		}

		artist := strings.TrimSpace(it.UserInfo.Name)
		if artist == "" {
			artist = "荔枝FM"
		}
		cover := strings.TrimSpace(it.VoiceInfo.ImageURL)
		if cover == "" {
			cover = strings.TrimSpace(it.UserInfo.Photo)
		}

		out = append(out, lzTrack{
			ID:       id,
			Name:     name,
			Artist:   artist,
			Cover:    cover,
			Duration: it.VoiceInfo.Duration,
			TrackURL: trackURL,
			Day:      day,
		})
	}
	return out, resp.ReceiptData, nil
}

// LizhiSearchLimit 供「荔枝」单源搜索用。
//
// 荔枝的检索偏「标题包含」，歌名+歌手拼一起对播客常常搜不到，
// 所以和 5sing 一样：拼起来没结果就退回只搜歌名。
func LizhiSearchLimit(song, singer string, limit int) []lzTrack {
	if !lizhiEnabled() {
		return nil
	}
	song, singer = strings.TrimSpace(song), strings.TrimSpace(singer)
	if limit <= 0 {
		return nil
	}
	if limit > 50 {
		limit = 50
	}

	run := func(kw string, grams map[string]bool) []lzTrack {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			return nil
		}
		all := make([]lzTrack, 0, limit*2)  // 过滤前的原始召回，回退用
		kept := make([]lzTrack, 0, limit)   // 过了相关性过滤的
		seen := make(map[string]bool, limit*2)
		receipt := ""
		for page := 1; page <= lizhiMaxPages; page++ {
			if len(kept) >= limit {
				break
			}
			list, next, err := lzSearchPage(kw, page, receipt)
			if err != nil {
				fmt.Printf("[lizhi] 搜索失败 %q 第 %d 页: %v\n", kw, page, err)
				break
			}
			for _, t := range list {
				if seen[t.ID] {
					continue
				}
				seen[t.ID] = true
				all = append(all, t)
				if len(kept) < limit && lzMatchQuery(grams, t) {
					kept = append(kept, t)
				}
			}
			// 游标没前进说明到底了，别空转
			if next == "" || next == receipt || len(list) == 0 {
				break
			}
			receipt = next
		}
		// 一条都不沾边时把原始召回交出去：这个词在荔枝上大概确实没有对口内容，
		// 让用户自己看比显示「什么都没搜到」更有用（和 5sing 过滤伴奏同一思路）
		if len(kept) == 0 {
			if len(all) > limit {
				return all[:limit]
			}
			return all
		}
		return kept
	}

	gramsAll := lzBigrams(song)
	for g := range lzBigrams(singer) {
		gramsAll[g] = true
	}

	kw := strings.TrimSpace(song + " " + singer)
	list := run(kw, gramsAll)
	if len(list) == 0 && song != "" && singer != "" {
		list = run(song, lzBigrams(song))
	}
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

// ---------- 取链 ----------

// LizhiTrackSrc 按 xid（lz:<voiceId>:<yyyymmdd>）取 mp3 直链。
//
// 先问详情接口拿 trackUrl（最准）；接口不通或没给地址时，
// 用 xid 里带的日期按 CDN 路径规则拼一个兜底直链。
func LizhiTrackSrc(xid string) (string, error) {
	voiceID, day := lzParseXID(xid)
	if voiceID == "" {
		return "", fmt.Errorf("缺少荔枝 voiceId")
	}
	if _, err := strconv.ParseInt(voiceID, 10, 64); err != nil {
		return "", fmt.Errorf("荔枝 voiceId 格式不对: %s", voiceID)
	}

	body, err := lizhiGet(lizhiInfoAPI + url.PathEscape(voiceID))
	if err == nil {
		var resp lzInfoResp
		if jerr := json.Unmarshal(body, &resp); jerr == nil && resp.Code == 0 {
			uv := resp.Data.UserVoice
			if u := lzHTTPS(uv.VoicePlayProperty.TrackURL); u != "" && strings.HasPrefix(u, "http") {
				return u, nil
			}
			// 详情接口没给直链：如果它带了 accessPermission=4，说明是付费内容，
			// 这时候拼兜底地址也拉不到，直接报清楚原因。
			if perm := uv.VoicePlayProperty.PlayAccessProperty.AccessPermission; perm != 0 && perm != 2 {
				return "", fmt.Errorf("该节目为付费内容，需在荔枝 App 内购买后才能收听")
			}
			if day == "" {
				day = lzDayFromURL(uv.VoiceInfo.ImageURL)
			}
		}
	} else {
		fmt.Printf("[lizhi] 详情接口失败 id=%s: %v（尝试按日期拼兜底地址）\n", voiceID, err)
	}

	if u := lzFallbackURL(voiceID, day); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("荔枝取链失败：没有可用的播放地址")
}
