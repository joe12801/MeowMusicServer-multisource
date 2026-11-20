package main

// mp3api 后台预热：用户点歌时，异步让 mp3.994938.xyz 把这首歌缓存起来，
// 方便以后链路兜底到 mp3api 时能秒回。要点：
//   - 只发一次 Range: bytes=0-65535 探测，读 64KB 就断开（约 16s，64KB 流量）。
//     上游服务端收到请求后会自行完成 yt-dlp 下载 + ffmpeg 转码并缓存 90 天，
//     我们不等它转完、不占跨境带宽，也不阻塞点歌。
//   - 点歌链路照常走前面的源（酷我等）立即返回；预热完全在后台 goroutine 里。
//   - 并发限流（最多 2 个）+ 同曲 10 分钟去重，避免堆积。
//   - 默认开启；MUSIC_MP3API_PREWARM=0 可关闭。
//   - 日志不打印完整 URL（KEY 用 mp3ApiMaskKey 打码）。

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	prewarmTTL     = 10 * time.Minute // 同一曲 10 分钟内只预热一次
	prewarmReadMax = 1 << 16          // 只读 64KB 就断开
)

var (
	prewarmSem  = make(chan struct{}, 2) // 最多 2 个并发预热
	prewarmSeen sync.Map                 // query -> time.Time
)

func mp3ApiPrewarmEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MUSIC_MP3API_PREWARM"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// mp3ApiPrewarmAsync 非阻塞：立即返回，真正的请求在 goroutine 里做。
// 在 requestAndCacheMusic 里以 defer 调用，保证命中任意源都会顺手预热。
func mp3ApiPrewarmAsync(song, singer string) {
	if !mp3ApiPrewarmEnabled() || mp3ApiKey() == "" {
		return
	}
	q := strings.TrimSpace(strings.Join(strings.Fields(song+" "+singer), " "))
	if q == "" {
		return
	}
	now := time.Now()
	if v, ok := prewarmSeen.Load(q); ok && now.Sub(v.(time.Time)) < prewarmTTL {
		return
	}
	prewarmSeen.Store(q, now)

	go func() {
		select {
		case prewarmSem <- struct{}{}:
			defer func() { <-prewarmSem }()
		default:
			fmt.Printf("[prewarm] skip %q (busy)\n", q)
			return
		}

		t0 := time.Now()
		audioURL := fmt.Sprintf("%s/api/music?q=%s&api_key=%s",
			mp3ApiBase(), url.QueryEscape(q), url.QueryEscape(mp3ApiKey()))
		req, err := http.NewRequest("GET", audioURL, nil)
		if err != nil {
			fmt.Printf("[prewarm] %q build request error: %v\n", q, err)
			return
		}
		req.Header.Set("User-Agent", "MeowMusicServer-prewarm/1.0")
		req.Header.Set("Range", "bytes=0-65535")

		resp, err := mp3ApiClient.Do(req)
		if err != nil {
			fmt.Printf("[prewarm] %q request error: %v\n", q, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			fmt.Printf("[prewarm] %q http %d\n", q, resp.StatusCode)
			return
		}
		// 只读一点就断开——触发上游后台转码/缓存即可，不把整首拉回来
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, prewarmReadMax))
		fmt.Printf("[prewarm] ✅ %q 已触发上游缓存 | CR=%s | %.1fs\n",
			q, mp3ApiMaskKey(resp.Header.Get("Content-Range")), time.Since(t0).Seconds())
	}()
}
