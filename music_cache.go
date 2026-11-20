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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 转码去重：同一首歌的并发请求只跑一次 ffmpeg
var musicTranscodeLocks sync.Map // cacheFile(string) -> *sync.Mutex

// cacheKey 由 singer-song 组成，与 streamLiveHandler 的缓存目录一致
func musicCacheFile(song, singer string) string {
	return filepath.Join("./files/cache/music", fmt.Sprintf("%s-%s", singer, song), "music.mp3")
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

	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err != nil {
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
		return fmt.Errorf("ffmpeg failed after %.1fs: %v | %s",
			time.Since(t0).Seconds(), err, tailString(stderr.String(), 240))
	}

	info, serr := os.Stat(tmpFile)
	if serr != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("transcoded file stat error: %v", serr)
	}
	if info.Size() < 10000 {
		os.Remove(tmpFile)
		return fmt.Errorf("transcoded file too small: %d bytes", info.Size())
	}
	if rerr := os.Rename(tmpFile, cacheFile); rerr != nil {
		os.Remove(tmpFile)
		return rerr
	}
	fmt.Printf("[Cache] transcoded %q -> %d bytes in %.1fs\n",
		filepath.Base(cacheFile), info.Size(), time.Since(t0).Seconds())
	return nil
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// serveCachedAudio 以明确的 Content-Length 返回完整音频（200，不用 chunked / 206）。
// 设备端 esp_http_client 对这种标准定长响应支持最好，按长度读完即结束。
func serveCachedAudio(w http.ResponseWriter, cacheFile string) error {
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
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, f)
	return err
}
