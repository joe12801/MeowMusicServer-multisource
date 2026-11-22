package main

// 稳健的音乐缓存层：完整转码到本地文件 + 用明确的 Content-Length 返回。
//
// 背景（2026-11-20 晚）：
//   - /stream_live 早期用 chunked 推流，ESP32 解析 chunk 边界失败 → 断流
//   - 改成 raw(Connection: close) 后，上游 mp3api 单曲从 1.1MB 涨到 7.3MB，
//     边下边转太慢，设备等待超时断开；设备断开后 ffmpeg 卡在 pipe 上，
//     而 stdout.Read 永久阻塞、检测不到客户端断开 → 累积僵尸 ffmpeg，
//     最终把上游也拖垮 → 设备彻底没声音。
//
// 本方案：
//   1) 现场请求时先把整首歌完整转码到本地缓存文件（.tmp 后原子 rename）
//   2) 转码用 -rw_timeout + context 超时双重保护，绝不永久挂起
//   3) per-song 互斥锁去重，避免并发重复转码 / 堆积进程
//   4) 缓存命中后用 Content-Length 明确长度返回完整文件（200，非 206/chunked），
//      ESP32 esp_http_client 按长度读完即结束，最稳
//   5) 缓存有就长期复用（配合上游 90 天缓存），越用越快

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 转码去重：同一首歌的并发请求只跑一次 ffmpeg
var musicTranscodeLocks sync.Map // cacheFile(string) -> *sync.Mutex

// 转码并发闸门。
//
// 这台 NAS 只有 2 个核，一首歌 32kbps 转码本身不算重，但 5~6 个请求同时
// 进来（设备重连、手机端拖进度条、后台下载）就能把 CPU 吃满，
// 连搜索/接口响应都被拖慢。这里限制同时跑的 ffmpeg 数量，
// 排队超过 2 分钟就直接失败——总比无限堆积进程强。
var (
	transcodeSemOnce sync.Once
	transcodeSem     chan struct{}
	transcodeActive  int64
	transcodeTotal   int64
	transcodeFail    int64
)

func transcodeLimit() int {
	n := envInt("TRANSCODE_MAX_CONCURRENCY", 2)
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return n
}

func transcodeSemaphore() chan struct{} {
	transcodeSemOnce.Do(func() {
		transcodeSem = make(chan struct{}, transcodeLimit())
	})
	return transcodeSem
}

func transcodeStats() map[string]interface{} {
	return map[string]interface{}{
		"active": atomic.LoadInt64(&transcodeActive),
		"limit":  transcodeLimit(),
		"total":  atomic.LoadInt64(&transcodeTotal),
		"failed": atomic.LoadInt64(&transcodeFail),
	}
}

// cacheKey 由 singer-song 组成，与 streamLiveHandler 的缓存目录一致
func musicCacheFile(song, singer string) string {
	return filepath.Join("./files/cache/music", cacheDirName(singer, song), "music.mp3")
}

// cacheDirName 生成缓存子目录名。
// 旧实现直接 fmt.Sprintf("%s-%s", singer, song)，song 里带 "../../x"
// 就能把文件写到缓存目录外面去，这里统一净化成安全的单层目录名。
func cacheDirName(singer, song string) string {
	name := sanitizeCacheSegment(singer) + "-" + sanitizeCacheSegment(song)
	r := []rune(name)
	if len(r) > 120 {
		name = string(r[:120])
	}
	return name
}

func sanitizeCacheSegment(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20:
			b.WriteRune('_')
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ". ")
	if out == "" {
		out = "unknown"
	}
	return out
}

// ensureMusicCache 保证 cacheFile 存在；不存在则完整转码 remoteURL 到该文件。
// 返回时要么缓存文件已就绪，要么 error（不会留下半文件）。
func ensureMusicCache(remoteURL, cacheFile string) error {
	if _, err := os.Stat(cacheFile); err == nil {
		return nil
	}

	lockI, _ := musicTranscodeLocks.LoadOrStore(cacheFile, &sync.Mutex{})
	lock := lockI.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	// 双重检查：等锁期间可能已被其它请求转好
	if _, err := os.Stat(cacheFile); err == nil {
		return nil
	}

	// 拿到并发闸门才真正开 ffmpeg
	sem := transcodeSemaphore()
	waitStart := time.Now()
	select {
	case sem <- struct{}{}:
	case <-time.After(120 * time.Second):
		atomic.AddInt64(&transcodeFail, 1)
		return fmt.Errorf("转码排队超时（并发上限 %d 已占满）", transcodeLimit())
	}
	defer func() { <-sem }()
	if waited := time.Since(waitStart); waited > 2*time.Second {
		fmt.Printf("[Transcode] 排队 %.1fs 后拿到槽位（上限 %d）\n", waited.Seconds(), transcodeLimit())
	}
	atomic.AddInt64(&transcodeActive, 1)
	atomic.AddInt64(&transcodeTotal, 1)
	defer atomic.AddInt64(&transcodeActive, -1)

	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err != nil {
		atomic.AddInt64(&transcodeFail, 1)
		return err
	}
	tmpFile := cacheFile + ".tmp"
	os.Remove(tmpFile) // 清理上次可能遗留的半文件

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-rw_timeout", "30000000", // 30s 无数据则判超时（微秒），防上游卡死
		"-i", remoteURL,
		"-threads", "0",
		"-ac", "1", "-ar", "24000", "-b:a", "32k", "-q:a", "9",
		"-f", "mp3", "-map_metadata", "-1", "-y", tmpFile)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	t0 := time.Now()
	err := cmd.Run()
	if err != nil {
		os.Remove(tmpFile)
		atomic.AddInt64(&transcodeFail, 1)
		return fmt.Errorf("ffmpeg failed after %.1fs: %v | %s",
			time.Since(t0).Seconds(), err, tailString(stderr.String(), 240))
	}

	info, serr := os.Stat(tmpFile)
	if serr != nil {
		os.Remove(tmpFile)
		atomic.AddInt64(&transcodeFail, 1)
		return fmt.Errorf("transcoded file stat error: %v", serr)
	}
	if info.Size() < 10000 {
		os.Remove(tmpFile)
		atomic.AddInt64(&transcodeFail, 1)
		return fmt.Errorf("transcoded file too small: %d bytes", info.Size())
	}
	if rerr := os.Rename(tmpFile, cacheFile); rerr != nil {
		os.Remove(tmpFile)
		atomic.AddInt64(&transcodeFail, 1)
		return rerr
	}
	fmt.Printf("[Cache] transcoded %q -> %d bytes in %.1fs\n",
		filepath.Base(cacheFile), info.Size(), time.Since(t0).Seconds())
	invalidateCacheSnapshot()
	// 转码完文件就在本地，顺手量一下真实时长（异步，不拖慢响应）
	go backfillCacheDuration(cacheFile)
	return nil
}

// ---------- 时长回填 ----------
//
// 各音源的 duration 经常是 0（搜索列表里进度条就是空的），而转码后的文件
// 就在本地，ffprobe 一次就能拿到准确时长。写进同目录 meta.json，
// 之后同一首歌再出现在搜索结果里就直接补上，不用再问音源。

type cacheMeta struct {
	Duration int    `json:"duration"` // 秒
	Size     int64  `json:"size"`
	At       string `json:"at"`
}

func cacheMetaPath(cacheFile string) string {
	return filepath.Join(filepath.Dir(cacheFile), "meta.json")
}

func readCacheMeta(cacheFile string) (cacheMeta, bool) {
	var m cacheMeta
	data, err := os.ReadFile(cacheMetaPath(cacheFile))
	if err != nil {
		return m, false
	}
	if json.Unmarshal(data, &m) != nil || m.Duration <= 0 {
		return m, false
	}
	return m, true
}

var (
	durationProbe        = make(chan struct{}, 1) // 同时只跑一个 ffprobe
	durationProbePending sync.Map                 // cacheFile -> struct{} 防重复排队
)

func probeDurationSeconds(file string) int {
	durationProbe <- struct{}{}
	defer func() { <-durationProbe }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", file).Output()
	if err != nil {
		return 0
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || sec <= 0 {
		return 0
	}
	return int(sec + 0.5)
}

func backfillCacheDuration(cacheFile string) {
	if _, ok := readCacheMeta(cacheFile); ok {
		return
	}
	if _, busy := durationProbePending.LoadOrStore(cacheFile, struct{}{}); busy {
		return
	}
	defer durationProbePending.Delete(cacheFile)

	info, err := os.Stat(cacheFile)
	if err != nil {
		return
	}
	sec := probeDurationSeconds(cacheFile)
	if sec <= 0 {
		return
	}
	data, _ := json.MarshalIndent(cacheMeta{
		Duration: sec,
		Size:     info.Size(),
		At:       time.Now().Format(time.RFC3339),
	}, "", " ")
	if err := writeFileAtomic(cacheMetaPath(cacheFile), data, 0o644); err != nil {
		return
	}
	fmt.Printf("[Cache] 回填时长 %s = %ds\n", filepath.Base(filepath.Dir(cacheFile)), sec)
}

// cachedDuration 查已缓存歌曲的真实时长（秒），0 表示还不知道。
func cachedDuration(song, singer string) int {
	if m, ok := readCacheMeta(musicCacheFile(song, singer)); ok {
		return m.Duration
	}
	return 0
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// serveCachedAudio 返回缓存音频。
// 无 Range 请求时返回定长 200（ESP32 esp_http_client 按长度读完即结束）；
// 带 Range 时返回 206 + 正确分段，浏览器拖进度条/续传不用重下整首。
// 旧实现声明了 Accept-Ranges: bytes 却从不处理 Range 头，永远 200 全量。
func serveCachedAudio(w http.ResponseWriter, r *http.Request, cacheFile string) error {
	f, err := os.Open(cacheFile)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Accept-Ranges", "bytes")
	if info.Size() > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	// ServeContent 在非 Range 请求下按 Content-Length 定长返回，
	// 只有客户端显式要 Range 时才走 206。
	http.ServeContent(w, r, filepath.Base(cacheFile), info.ModTime(), f)
	return nil
}
