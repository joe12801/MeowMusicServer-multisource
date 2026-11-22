package main

// 缓存自动回收
//
// 之前 data/files/cache 只增不减：一首歌转完约 1.1MB，一年多下来
// 会把 NAS 的盘吃干净，而且没有任何地方能看到它占了多少。
//
// 策略（都可关，见 .env 里的 CACHE_*）：
//   1. TTL：超过 CACHE_TTL_DAYS 天没被访问的音频删掉（默认 45 天）
//   2. 容量：总量超过 CACHE_MAX_GB 时，按最后修改时间从旧到新删到 90%（默认 20GB）
//   3. 半成品：转码中断留下的 *.tmp 超过 2 小时一律清掉
//
// 删文件不会影响正在播放的请求——Linux 下已打开的 fd 仍然可读，
// 只是目录项消失，下次点同一首会重新转码。

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	musicCacheRoot = "./files/cache/music"
	lyricCacheRoot = "./files/cache/lyrics"

	cacheGCInterval = 30 * time.Minute
)

type cacheEntry struct {
	path string
	mod  time.Time
	size int64
}

type cacheStat struct {
	MusicFiles     int64  `json:"music_files"`
	MusicBytes     int64  `json:"music_bytes"`
	LyricFiles     int64  `json:"lyric_files"`
	LyricBytes     int64  `json:"lyric_bytes"`
	MaxBytes       int64  `json:"max_bytes"`
	TTLDays        int    `json:"ttl_days"`
	LastGC         string `json:"last_gc,omitempty"`
	LastFreedBytes int64  `json:"last_freed_bytes"`
	LastDeleted    int    `json:"last_deleted"`
	DiskFreeBytes  int64  `json:"disk_free_bytes"`
	DiskTotalBytes int64  `json:"disk_total_bytes"`
}

var (
	cacheGCmu       sync.Mutex
	cacheSnapshotMu sync.Mutex
	cacheSnapshotAt time.Time
	cacheSnapshotV  cacheStat
	lastGCAt        time.Time
	lastGCFreed     int64
	lastGCDeleted   int
)

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func cacheTTLDays() int {
	return envInt("CACHE_TTL_DAYS", 45)
}

func cacheMaxBytes() int64 {
	gb := envInt("CACHE_MAX_GB", 20)
	if gb <= 0 {
		return 0
	}
	return int64(gb) << 30
}

// scanCacheDir 统计目录占用；collect=true 时同时返回每个文件的元信息用于回收。
func scanCacheDir(root string, collect bool) (files int64, bytes int64, entries []cacheEntry) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files++
		bytes += info.Size()
		if collect && !strings.HasSuffix(p, ".tmp") {
			entries = append(entries, cacheEntry{path: p, mod: info.ModTime(), size: info.Size()})
		}
		return nil
	})
	return files, bytes, entries
}

// cacheSnapshot 对外暴露缓存占用。扫描结果缓存 60 秒，避免面板反复刷盘。
func cacheSnapshot() cacheStat {
	cacheSnapshotMu.Lock()
	defer cacheSnapshotMu.Unlock()

	if time.Since(cacheSnapshotAt) < time.Minute && !cacheSnapshotAt.IsZero() {
		return cacheSnapshotV
	}

	mf, mb, _ := scanCacheDir(musicCacheRoot, false)
	lf, lb, _ := scanCacheDir(lyricCacheRoot, false)

	st := cacheStat{
		MusicFiles:     mf,
		MusicBytes:     mb,
		LyricFiles:     lf,
		LyricBytes:     lb,
		MaxBytes:       cacheMaxBytes(),
		TTLDays:        cacheTTLDays(),
		LastFreedBytes: lastGCFreed,
		LastDeleted:    lastGCDeleted,
	}
	if !lastGCAt.IsZero() {
		st.LastGC = lastGCAt.Format("2006-01-02 15:04:05")
	}
	st.DiskFreeBytes, st.DiskTotalBytes = diskUsage(".")

	cacheSnapshotV = st
	cacheSnapshotAt = time.Now()
	return st
}

func invalidateCacheSnapshot() {
	cacheSnapshotMu.Lock()
	cacheSnapshotAt = time.Time{}
	cacheSnapshotMu.Unlock()
}

// startCacheGC 起后台回收协程；首次延迟 2 分钟，之后每 30 分钟一次。
func startCacheGC() {
	go func() {
		time.Sleep(2 * time.Minute)
		runCacheGC()
		t := time.NewTicker(cacheGCInterval)
		defer t.Stop()
		for range t.C {
			runCacheGC()
		}
	}()
}

func runCacheGC() {
	if !cacheGCmu.TryLock() {
		return
	}
	defer cacheGCmu.Unlock()

	defer func() {
		if rec := recover(); rec != nil {
			fmt.Printf("[CacheGC] panic: %v\n", rec)
		}
	}()

	now := time.Now()
	ttlDays := cacheTTLDays()
	maxBytes := cacheMaxBytes()

	var deleted int
	var freed int64

	// 1) 清理转码中断留下的 .tmp
	filepath.WalkDir(musicCacheRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".tmp") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && now.Sub(info.ModTime()) > 2*time.Hour {
			if os.Remove(p) == nil {
				deleted++
				freed += info.Size()
			}
		}
		return nil
	})

	files, bytes, entries := scanCacheDir(musicCacheRoot, true)

	// 2) TTL
	if ttlDays > 0 {
		cutoff := now.AddDate(0, 0, -ttlDays)
		kept := entries[:0]
		for _, e := range entries {
			if e.mod.Before(cutoff) {
				if os.Remove(e.path) == nil {
					deleted++
					freed += e.size
					bytes -= e.size
					files--
					continue
				}
			}
			kept = append(kept, e)
		}
		entries = kept
	}

	// 3) 容量上限：从最旧的开始删，删到 90% 以下
	if maxBytes > 0 && bytes > maxBytes {
		sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
		target := maxBytes / 10 * 9
		for _, e := range entries {
			if bytes <= target {
				break
			}
			if os.Remove(e.path) == nil {
				deleted++
				freed += e.size
				bytes -= e.size
				files--
			}
		}
		fmt.Printf("[CacheGC] 超出容量上限 %d MB，已回收到 %d MB\n", maxBytes>>20, bytes>>20)
	}

	pruneEmptyDirs(musicCacheRoot)

	if deleted > 0 {
		fmt.Printf("[CacheGC] 删除 %d 个文件，释放 %.1f MB，剩余 %d 首 / %.1f MB\n",
			deleted, float64(freed)/(1<<20), files, float64(bytes)/(1<<20))
	}

	lastGCAt = now
	lastGCFreed = freed
	lastGCDeleted = deleted
	invalidateCacheSnapshot()
}

func pruneEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	// 由深到浅删，避免父目录还因子目录非空而留下
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		os.Remove(d) // 目录非空时自动失败，正是想要的行为
	}
}
