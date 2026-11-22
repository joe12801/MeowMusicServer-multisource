package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// APIHandler handles API requests.
func apiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "MeowMusicEmbeddedServer")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	queryParams := r.URL.Query()
	fmt.Printf("[Web Access] Handling request for %s?%s\n", r.URL.Path, queryParams.Encode())
	song := queryParams.Get("song")
	singer := queryParams.Get("singer")

	ip, err := IPhandler(r)
	if err != nil {
		ip = "0.0.0.0"
	}

	if song == "" {
		musicItem := MusicItem{
			FromCache: false,
			IP:        ip,
		}
		json.NewEncoder(w).Encode(musicItem)
		return
	}

	// Attempt to retrieve music items from sources.json
	sources := readSources()

	var musicItem MusicItem
	var found bool = false

	// Build request scheme
	var scheme string
	if r.TLS == nil {
		scheme = "http"
	} else {
		scheme = "https"
	}

	for _, source := range sources {
		if source.Title == song {
			if singer == "" || source.Artist == singer {
				// Determine the protocol for each URL and build accordingly
				var audioURL, audioFullURL, m3u8URL, lyricURL, coverURL string
				if strings.HasPrefix(source.AudioURL, "http://") {
					audioURL = scheme + "://" + r.Host + "/url/http/" + url.QueryEscape(strings.TrimPrefix(source.AudioURL, "http://"))
				} else if strings.HasPrefix(source.AudioURL, "https://") {
					audioURL = scheme + "://" + r.Host + "/url/https/" + url.QueryEscape(strings.TrimPrefix(source.AudioURL, "https://"))
				} else {
					audioURL = scheme + "://" + r.Host + "/" + url.QueryEscape(source.AudioURL)
				}
				if strings.HasPrefix(source.AudioFullURL, "http://") {
					audioFullURL = scheme + "://" + r.Host + "/url/http/" + url.QueryEscape(strings.TrimPrefix(source.AudioFullURL, "http://"))
				} else if strings.HasPrefix(source.AudioFullURL, "https://") {
					audioFullURL = scheme + "://" + r.Host + "/url/https/" + url.QueryEscape(strings.TrimPrefix(source.AudioFullURL, "https://"))
				} else {
					audioFullURL = scheme + "://" + r.Host + "/" + url.QueryEscape(source.AudioFullURL)
				}
				if strings.HasPrefix(source.M3U8URL, "http://") {
					m3u8URL = scheme + "://" + r.Host + "/url/http/" + url.QueryEscape(strings.TrimPrefix(source.M3U8URL, "http://"))
				} else if strings.HasPrefix(source.M3U8URL, "https://") {
					m3u8URL = scheme + "://" + r.Host + "/url/https/" + url.QueryEscape(strings.TrimPrefix(source.M3U8URL, "https://"))
				} else {
					m3u8URL = scheme + "://" + r.Host + "/" + url.QueryEscape(source.M3U8URL)
				}
				if strings.HasPrefix(source.LyricURL, "http://") {
					lyricURL = scheme + "://" + r.Host + "/url/http/" + url.QueryEscape(strings.TrimPrefix(source.LyricURL, "http://"))
				} else if strings.HasPrefix(source.LyricURL, "https://") {
					lyricURL = scheme + "://" + r.Host + "/url/https/" + url.QueryEscape(strings.TrimPrefix(source.LyricURL, "https://"))
				} else {
					lyricURL = scheme + "://" + r.Host + "/" + url.QueryEscape(source.LyricURL)
				}
				if strings.HasPrefix(source.CoverURL, "http://") {
					coverURL = scheme + "://" + r.Host + "/url/http/" + url.QueryEscape(strings.TrimPrefix(source.CoverURL, "http://"))
				} else if strings.HasPrefix(source.CoverURL, "https://") {
					coverURL = scheme + "://" + r.Host + "/url/https/" + url.QueryEscape(strings.TrimPrefix(source.CoverURL, "https://"))
				} else {
					coverURL = scheme + "://" + r.Host + "/" + url.QueryEscape(source.CoverURL)
				}
				musicItem = MusicItem{
					Title:        source.Title,
					Artist:       source.Artist,
					AudioURL:     audioURL,
					AudioFullURL: audioFullURL,
					M3U8URL:      m3u8URL,
					LyricURL:     lyricURL,
					CoverURL:     coverURL,
					Duration:     source.Duration,
					FromCache:    false,
				}
				found = true
				break
			}
		}
	}

	// If not found in sources.json, attempt to retrieve from local folder
	if !found {
		musicItem = getLocalMusicItem(song, singer)
		musicItem.FromCache = false
		if musicItem.Title != "" {
			if musicItem.AudioURL != "" {
				musicItem.AudioURL = scheme + "://" + r.Host + musicItem.AudioURL
			}
			if musicItem.AudioFullURL != "" {
				musicItem.AudioFullURL = scheme + "://" + r.Host + musicItem.AudioFullURL
			}
			if musicItem.M3U8URL != "" {
				musicItem.M3U8URL = scheme + "://" + r.Host + musicItem.M3U8URL
			}
			if musicItem.LyricURL != "" {
				musicItem.LyricURL = scheme + "://" + r.Host + musicItem.LyricURL
			}
			if musicItem.CoverURL != "" {
				musicItem.CoverURL = scheme + "://" + r.Host + musicItem.CoverURL
			}
			found = true
		}
	}

	// If still not found, 看本地转码缓存里有没有这首歌。
	// 旧实现 glob 的是 ./cache/*.json（实际缓存在 ./files/cache），
	// 还把 JSON 文件当目录读，永远命中不了，是纯死代码。
	// 现在直接查真实的缓存音频文件，命中就给静态地址，省一次取链+转码。
	if !found {
		if _, statErr := os.Stat(musicCacheFile(song, singer)); statErr == nil {
			fmt.Println("[Info] Reading music from local transcode cache.")
			rel := "/cache/music/" + url.PathEscape(cacheDirName(singer, song)) + "/music.mp3"
			abs := scheme + "://" + r.Host + rel
			musicItem = MusicItem{
				Title:        song,
				Artist:       singer,
				AudioURL:     abs,
				AudioFullURL: abs,
				FromCache:    true,
			}
			found = true
		}
	}

	// If still not found, request and cache the music item in a separate goroutine
	// 直接进行流式播放
	if !found {
		encodedSong := url.QueryEscape(song)
		encodedSinger := url.QueryEscape(singer)
		streamURL := scheme + "://" + r.Host + "/stream_live?song=" + encodedSong + "&singer=" + encodedSinger
		fmt.Println("[Info] Updating music item cache from API request.")
		musicItem = requestAndCacheMusic(song, singer)
		fmt.Println("[Info] Music item cache updated.")
		musicItem.FromCache = false
		musicItem.AudioURL = streamURL
		musicItem.AudioFullURL = streamURL
		if musicItem.M3U8URL != "" && !strings.HasPrefix(musicItem.M3U8URL, "http") {
			musicItem.M3U8URL = scheme + "://" + r.Host + musicItem.M3U8URL
		}
		if musicItem.LyricURL != "" && !strings.HasPrefix(musicItem.LyricURL, "http") {
			musicItem.LyricURL = scheme + "://" + r.Host + musicItem.LyricURL
		}
		if musicItem.CoverURL != "" && !strings.HasPrefix(musicItem.CoverURL, "http") {
			musicItem.CoverURL = scheme + "://" + r.Host + musicItem.CoverURL
		}
		found = true
	}

	// If still not found, return an empty MusicItem
	if !found {
		musicItem = MusicItem{
			FromCache: false,
			IP:        ip,
		}
	} else {
		musicItem.IP = ip
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.Encode(musicItem)
}

// streamLiveHandler 实时流式转码接口 - 边下载边播放，无需等待！
func streamLiveHandler(w http.ResponseWriter, r *http.Request) {
	// 设置 CORS 和音频相关头
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Accept-Ranges", "bytes")

	queryParams := r.URL.Query()
	song := queryParams.Get("song")
	singer := queryParams.Get("singer")
	// xid：上游 id。喜马拉雅是纯数字 trackId，5sing 是 fs:<songId>:<typeEname>。
	// 这类内容（有声书/播客/原创翻唱）和音乐源完全不重叠，只能按 id 直取，
	// 不能靠歌名二次解析，否则会放成同名的歌。
	xid := strings.TrimSpace(queryParams.Get("xid"))
	// bv：B 站的 BV 号。同理——UP 主上传的现场/二创和音乐源不重叠，
	// 按 BV 号取流（cid 在播放时再用 view 接口换），不跑多源链路。
	bv := strings.TrimSpace(queryParams.Get("bv"))

	fmt.Printf("[Stream Live] Request: song=%s, singer=%s, xid=%s, bv=%s\n", song, singer, xid, bv)

	if song == "" {
		http.Error(w, "Missing song parameter", http.StatusBadRequest)
		return
	}

	// 游客试听额度：只对「从头开始的那次请求」检查和计数。
	// 拖动进度条、断线续传的 Range 请求不算新的一首，也不会被半路截断。
	if countsAsNewPlay(r) && isGuestRequest(r) {
		if _, gerr := checkGuestQuota(r); gerr != nil {
			fmt.Printf("[Stream Live] 游客额度用尽（%s）：%v\n", clientIP(r), gerr)
			w.Header().Set("Retry-After", "3600")
			w.Header().Set("X-Guest-Quota", "exhausted")
			http.Error(w, gerr.Error(), http.StatusTooManyRequests)
			return
		}
		noteGuestPlay(r)
	}

	// 缓存路径：./files/cache/music/<singer>-<song>/music.mp3
	cachedFile := musicCacheFile(song, singer)

	// 1. 缓存命中：定长返回（带 Content-Length，设备按长度读完即结束）
	if _, err := os.Stat(cachedFile); err == nil {
		fmt.Printf("[Stream Live] Serving from cache: %s\n", cachedFile)
		// 老缓存没有 meta.json：顺手量一次时长，之后搜索能补上 duration
		if _, ok := readCacheMeta(cachedFile); !ok {
			go backfillCacheDuration(cachedFile)
		}
		if err := serveCachedAudio(w, r, cachedFile); err != nil {
			fmt.Printf("[Stream Live] Serve cache error: %v\n", err)
		}
		return
	}

	// 2. 缓存不存在：先查每日额度，超了直接 429，不启动 ffmpeg
	if err := checkTranscodeQuota(r); err != nil {
		fmt.Printf("[Stream Live] 额度不足: %v\n", err)
		w.Header().Set("Retry-After", "3600")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}

	fmt.Printf("[Stream Live] Cache miss, fetching from API...\n")

	var remoteURL, srcName string
	// refetch：转码失败后重新取一次直链（限流/节点问题才需要）。
	// 用闭包是为了让 B 站能同时用上「备用 CDN 地址」和「重新签一次 URL」两条路，
	// 也让多源链路保持原来的 refetchRemoteURL 逻辑不变。
	var refetch func() string
	switch {
	case bv != "":
		// B 站：按 BV 号取纯音频直链，不跑多源链路。
		// 失效视频会自动换一条同名的能播版本（返回实际用上的 BV）。
		srcName = sourceBilibili
		audio, usedBV, err := BilibiliResolve(bv, song, singer)
		if err != nil {
			fmt.Printf("[Stream Live] B站取链失败 bv=%s: %v\n", bv, err)
			w.Header().Set("X-Source", sourceBilibili)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		bv = usedBV
		remoteURL = audio.URL
		if audio.Title != "" {
			quality := ""
			if audio.Lossless {
				quality = " 无损"
			}
			fmt.Printf("[Stream Live] B站命中：%s - %s（%dkbps%s）\n",
				audio.Title, audio.Artist, audio.Bandwidth/1000, quality)
		}
		backups := audio.Backups
		refetch = func() string {
			for len(backups) > 0 {
				next := backups[0]
				backups = backups[1:]
				if next != "" {
					return next
				}
			}
			// 备用地址也全灭了，就重新签一次（B 站直链带有效期，重签能换节点）
			if again, err := BilibiliAudio(bv); err == nil {
				backups = again.Backups
				return again.URL
			}
			return ""
		}
	case xid != "":
		// 按 xid 直取，不跑多源链路。xid 前缀区分来源：
		//   fs:...  → 5sing（songId + typeEname）
		//   lz:...  → 荔枝 FM（voiceId + 日期）
		//   纯数字  → 喜马拉雅 trackId
		// 荔枝必须靠前缀认：它的 voiceId 是纯数字，和喜马拉雅的 trackId 同形。
		var (
			src string
			err error
		)
		switch {
		case strings.HasPrefix(xid, fiveSingXIDPrefix):
			srcName = sourceFiveSing
			src, err = FiveSingTrackSrc(xid)
			if err != nil {
				fmt.Printf("[Stream Live] 5sing 取链失败 xid=%s: %v\n", xid, err)
				w.Header().Set("X-Source", sourceFiveSing)
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		case strings.HasPrefix(xid, lizhiXIDPrefix):
			srcName = sourceLizhi
			src, err = LizhiTrackSrc(xid)
			if err != nil {
				fmt.Printf("[Stream Live] 荔枝取链失败 xid=%s: %v\n", xid, err)
				w.Header().Set("X-Source", sourceLizhi)
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		default:
			srcName = sourceXimalaya
			src, err = XimalayaTrackSrc(xid)
			if err != nil {
				fmt.Printf("[Stream Live] 喜马拉雅取链失败 xid=%s: %v\n", xid, err)
				w.Header().Set("X-Source", sourceXimalaya)
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		remoteURL = src
		// 荔枝直链不带签名，cdn5 / cdn101 是两条线路，
		// 一条拉不动时换另一条通常能过（实测 cdn5 更快，所以它在前）。
		lzBackups := lizhiBackupURLs(remoteURL)
		refetch = func() string {
			for len(lzBackups) > 0 {
				next := lzBackups[0]
				lzBackups = lzBackups[1:]
				if next != "" {
					return next
				}
			}
			return ""
		}
	default:
		remoteURL, srcName = getRemoteMusicURLWithSource(song, singer)
		refetch = func() string { return refetchRemoteURL(song, singer, srcName) }
	}
	if remoteURL == "" {
		http.Error(w, "Failed to get remote music URL", http.StatusNotFound)
		return
	}

	// B 站和荔枝的音频 CDN 都认浏览器 UA，转码时得把头一起传下去
	var streamHeaders map[string]string
	switch srcName {
	case sourceBilibili:
		streamHeaders = biliStreamHeaders()
	case sourceLizhi:
		streamHeaders = lizhiStreamHeaders()
	}

	// 转码失败重试：只对「直链会换节点」的源有意义（汽水、B站）。
	// 境外实测汽水约 1/3 概率落到不通的节点（v11-luna 整段超时 → ffmpeg exit 146），
	// 重取一次直链大概率换到 v3/v5/v26 这类可达节点，重试两次后成功率约 95%。
	// B 站直链带有效期且多备选 CDN，失败时换备用地址/重签一次同样有用。
	// 其它源直链固定，失败就是真失败，直接返回 500，不做无意义的重试。
	const maxTranscodeAttempts = 3
	var transcodeErr error
	for attempt := 1; attempt <= maxTranscodeAttempts; attempt++ {
		fmt.Printf("[Stream Live] Transcoding from: %s（第 %d 次，源=%q）\n",
			mp3ApiMaskKey(remoteURL), attempt, srcName)

		transcodeErr = ensureMusicCacheWithHeaders(remoteURL, cachedFile, streamHeaders)
		if transcodeErr == nil {
			break
		}
		if attempt >= maxTranscodeAttempts || !retryableTranscodeSource(srcName) {
			break
		}

		fmt.Printf("[Stream Live] 转码失败（%v），重新取链换节点\n", transcodeErr)
		if next := refetch(); next != "" {
			remoteURL = next
		} else {
			// 新链没取到就沿用旧链再试一次，别提前放弃
			fmt.Printf("[Stream Live] 重新取链为空，沿用原直链重试\n")
		}
	}
	if transcodeErr != nil {
		fmt.Printf("[Stream Live] Transcode error: %v\n", transcodeErr)
		http.Error(w, "Transcode failed", http.StatusInternalServerError)
		return
	}

	// 转码成功才记账（命中缓存不算额度，重复听同一首不会消耗）
	if info, err := os.Stat(cachedFile); err == nil {
		noteTranscode(r, info.Size())
	}

	// 定长返回给设备
	if err := serveCachedAudio(w, r, cachedFile); err != nil {
		fmt.Printf("[Stream Live] Serve error: %v\n", err)
	}
}
