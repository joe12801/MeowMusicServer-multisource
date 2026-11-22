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

	fmt.Printf("[Stream Live] Request: song=%s, singer=%s\n", song, singer)

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

	remoteURL := getRemoteMusicURLOnly(song, singer)
	if remoteURL == "" {
		http.Error(w, "Failed to get remote music URL", http.StatusNotFound)
		return
	}

	fmt.Printf("[Stream Live] Transcoding from: %s\n", mp3ApiMaskKey(remoteURL))

	// 完整转码到本地缓存（带超时 + 去重锁，不会因设备断开而挂死）
	if err := ensureMusicCache(remoteURL, cachedFile); err != nil {
		fmt.Printf("[Stream Live] Transcode error: %v\n", err)
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
