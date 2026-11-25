package main

// mp3.994938.xyz —— yt-dlp + ffmpeg 的音乐音频接口（自建服务）
//
// 用法（用户指定，极简）：GET /api/music?q=<搜索关键词>&api_key=<KEY>
//   - 服务端自动搜索并取第一条，下载转码成 MP3 后返回 audio/mpeg 二进制流
//   - 首次（未缓存）要 yt-dlp 下载 + ffmpeg 转码，实测首字节 13~14s；
//     之后同关键词直接命中缓存，首字节 0.3~0.9s，支持 Range
//   - 返回的是音频流而不是 JSON，拿不到歌名/歌手/时长元数据，
//     所以 MusicItem 直接用用户点歌的原文，Duration 置 0
//
// 实测可用性（2026-11-20）：
//   - 本机（走代理）与境外 jd5 服务器直连都 200，
//     境外 health 0.24s / search 1.01s / 音频首字节 0.25s，比境内还快
//   - 无 KEY / 错 KEY 统一 401
//
// 定位：后段兜底，GDStudio 之后、yuafeng 之前。冷启动 13s+ 不适合前排；
// 但 YouTube 曲库覆盖广、国内外服务器都能直连，前面全落空时用它兜住。
//
// KEY 从环境变量 MUSIC_MP3API_KEY 读取，不硬编码、不进 git；
// 未配置时本源自动静默跳过。BASE 可用 MUSIC_MP3API_BASE 覆盖。

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const mp3ApiBaseDefault = "https://mp3.994938.xyz"

// 冷启动 = 对方 yt-dlp 下载 + ffmpeg 转码，实测 13~14s，留足余量
var mp3ApiClient = &http.Client{Timeout: 150 * time.Second}

func mp3ApiBase() string {
	if b := strings.TrimSpace(os.Getenv("MUSIC_MP3API_BASE")); b != "" {
		return strings.TrimRight(b, "/")
	}
	return mp3ApiBaseDefault
}

func mp3ApiKey() string {
	if k := strings.TrimSpace(os.Getenv("MUSIC_MP3API_KEY")); k != "" {
		return k
	}
	// 环境变量没配时，用 /tools 页面里保存的 KEY（即时生效，不用重启）
	return storedMP3ApiKey()
}

// mp3ApiFirst 由 MUSIC_MP3API_FIRST=1 开启：把本源临时提到整条链路最前。
// 用于「暂时优先试这个源」的场景，其它源仍保留在后面兜底；去掉环境变量
// 重启即恢复原顺序，不需要重新编译或回滚二进制。
func mp3ApiFirst() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MUSIC_MP3API_FIRST"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// mp3ApiMaskKey 日志打码：把 URL 里的 api_key 参数换成 ****，避免 KEY 落进服务日志
func mp3ApiMaskKey(u string) string {
	key := mp3ApiKey()
	if key == "" {
		return u
	}
	return strings.ReplaceAll(u, url.QueryEscape(key), "****")
}

// MP3ApiSearch 按关键词直接搜（取第一条），返回可被 ffmpeg 直接播放的音频 URL。
// 为了保证「真的能出声」才算命中，先探一次流（上游会顺便完成下载+转码并缓存，
// 之后 ffmpeg 正式拉取就是秒回），探不出来就返回空，让链路继续往后走。
func MP3ApiSearch(song, singer string) MusicItem {
	key := mp3ApiKey()
	if key == "" {
		return MusicItem{}
	}

	q := strings.TrimSpace(strings.Join(strings.Fields(song+" "+singer), " "))
	audioURL := fmt.Sprintf("%s/api/music?q=%s&api_key=%s",
		mp3ApiBase(), url.QueryEscape(q), url.QueryEscape(key))

	req, err := http.NewRequest("GET", audioURL, nil)
	if err != nil {
		return MusicItem{}
	}
	req.Header.Set("User-Agent", "MeowMusicServer/1.0")
	req.Header.Set("Range", "bytes=0-65535") // 只探 64KB，确认能出音频即可

	t0 := time.Now()
	resp, err := mp3ApiClient.Do(req)
	if err != nil {
		fmt.Printf("[mp3api] %q request error: %v\n", q, err)
		return MusicItem{}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		fmt.Printf("[mp3api] %q http %d: %s\n", q, resp.StatusCode, strings.TrimSpace(string(body)))
		return MusicItem{}
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(strings.ToLower(ct), "audio") &&
		!strings.Contains(strings.ToLower(ct), "octet-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		fmt.Printf("[mp3api] %q non-audio response (%s): %s\n", q, ct, strings.TrimSpace(string(body)))
		return MusicItem{}
	}
	// 丢弃探测流，正式播放由 ffmpeg 重新拉（此时上游已缓存，秒回）
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	fmt.Printf("[mp3api] ✅ %q | %s | %s bytes | %.1fs\n",
		q, ct, resp.Header.Get("Content-Length"), time.Since(t0).Seconds())

	return MusicItem{
		Title:    song,
		Artist:   singer,
		AudioURL: audioURL,
		Source:   "mp3api",
	}
}
