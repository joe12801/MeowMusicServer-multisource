package main

// 数据自动备份 + 夜间预热缓存
//
// 备份：用户的账号、会话、歌单、设备绑定全是单个 JSON 文件，写坏了就没法登录。
// 每天凌晨（默认 3 点）打一个 tar.gz 存到 files/backups/，保留最近 7 份。
// 用 Go 自带的 tar/gzip，不依赖宿主有没有装 tar。
//
// 预热：夜里把「我喜欢」和歌单里的歌提前转成本地缓存，第二天第一首不用等
// 十几秒 ffmpeg。串行跑、每次最多 PREWARM_TOP_N 首（默认 10），已经缓存过的跳过。
//
// 环境变量：
//   BACKUP_ENABLED=1        0 关闭自动备份
//   BACKUP_HOUR=3           每天几点备份（0-23）
//   BACKUP_KEEP=7           保留几份
//   PREWARM_TOP_N=10        每晚预热几首，0 关闭
//   PREWARM_HOUR=4          预热时间（默认 4 点，排在备份之后）

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const backupDir = "./files/backups"

type backupFileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	At   string `json:"at"`
}

var backupState = struct {
	sync.Mutex
	LastAt    time.Time
	LastName  string
	LastBytes int64
	LastErr   string
	Running   bool
}{}

// backupTargets 相对工作目录（容器内 /app）。不存在的会跳过。
var backupTargets = []string{
	"files/users.json",
	"files/sessions.json",
	"files/user_playlists.json",
	"files/playlists.json",
	"files/history.json",
	"files/usage.json",
	"devices.json",
	"sources.json",
}

func envFlag(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// runBackup 打一个包，成功返回文件名。
func runBackup() (string, error) {
	backupState.Lock()
	if backupState.Running {
		backupState.Unlock()
		return "", fmt.Errorf("备份正在进行中")
	}
	backupState.Running = true
	backupState.Unlock()

	defer func() {
		backupState.Lock()
		backupState.Running = false
		backupState.Unlock()
	}()

	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	name := "meowmusic-" + time.Now().Format("2006-01-02-150405") + ".tar.gz"
	finalPath := filepath.Join(backupDir, name)
	tmpPath := finalPath + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var packed int
	for _, rel := range backupTargets {
		st, err := os.Stat(rel)
		if err != nil || st.IsDir() {
			continue
		}
		src, err := os.Open(rel)
		if err != nil {
			continue
		}
		hdr := &tar.Header{
			Name:    rel,
			Mode:    0o600,
			Size:    st.Size(),
			ModTime: st.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			src.Close()
			continue
		}
		if _, err := io.Copy(tw, src); err != nil {
			src.Close()
			continue
		}
		src.Close()
		packed++
	}

	// 顺手写一份清单，恢复时不用猜包里有什么
	manifest := fmt.Sprintf("created=%s\nhost_time=%s\nfiles=%d\n",
		time.Now().Format(time.RFC3339), time.Now().Format("2006-01-02 15:04:05"), packed)
	tw.WriteHeader(&tar.Header{Name: "MANIFEST.txt", Mode: 0o600, Size: int64(len(manifest))})
	tw.Write([]byte(manifest))

	if err := tw.Close(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", err
	}
	f.Close()

	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	var size int64
	if st, err := os.Stat(finalPath); err == nil {
		size = st.Size()
	}
	pruneBackups(envInt("BACKUP_KEEP", 7))

	backupState.Lock()
	backupState.LastAt = time.Now()
	backupState.LastName = name
	backupState.LastBytes = size
	backupState.LastErr = ""
	backupState.Unlock()

	fmt.Printf("[Backup] ✅ %s（%d 个文件，%.1f KB）\n", name, packed, float64(size)/1024)
	return name, nil
}

// pruneBackups 只留最近 keep 份。
func pruneBackups(keep int) {
	if keep <= 0 {
		keep = 7
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return
	}
	type item struct {
		name string
		mod  time.Time
	}
	var list []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, item{name: e.Name(), mod: info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	for i := keep; i < len(list); i++ {
		if err := os.Remove(filepath.Join(backupDir, list[i].name)); err == nil {
			fmt.Printf("[Backup] 清理旧备份 %s\n", list[i].name)
		}
	}
}

// listBackups 返回现有备份，最新在前。
func listBackups() []backupFileInfo {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return []backupFileInfo{}
	}
	var out []backupFileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupFileInfo{
			Name: e.Name(),
			Size: info.Size(),
			At:   info.ModTime().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// backupStatus 给 /status 与 /api/backup 用。
func backupStatus() map[string]interface{} {
	backupState.Lock()
	last := backupState.LastAt
	lastErr := backupState.LastErr
	running := backupState.Running
	lastName := backupState.LastName
	lastBytes := backupState.LastBytes
	backupState.Unlock()

	list := listBackups()
	out := map[string]interface{}{
		"enabled":       envFlag("BACKUP_ENABLED", true),
		"hour":          envInt("BACKUP_HOUR", 3),
		"keep":          envInt("BACKUP_KEEP", 7),
		"running":       running,
		"files":         list,
		"count":         len(list),
		"last_error":    lastErr,
		"prewarm_n":     envInt("PREWARM_TOP_N", 10),
		"prewarm_known": prewarmKnownCount(),
	}
	if !last.IsZero() {
		out["last_at"] = last.Format("2006-01-02 15:04:05")
		out["last_name"] = lastName
		out["last_bytes"] = lastBytes
	} else if len(list) > 0 {
		// 进程刚启动时内存里没有记录，但磁盘上有备份文件——别显示「从未备份」
		out["last_at"] = list[0].At
		out["last_name"] = list[0].Name
		out["last_bytes"] = list[0].Size
	}
	return out
}

// HandleBackup 查询备份状态。
func HandleBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(backupStatus()); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// HandleBackupRun 立刻备份一次（运维页面按钮用）。
func HandleBackupRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, err := runBackup()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		backupState.Lock()
		backupState.LastErr = err.Error()
		backupState.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "name": name})
}

/* ================= 预热 ================= */

var prewarmKnown struct {
	sync.Mutex
	n int
}

func prewarmKnownCount() int {
	prewarmKnown.Lock()
	defer prewarmKnown.Unlock()
	return prewarmKnown.n
}

// warmFavorites 挑还没缓存的歌提前转码。
func warmFavorites() {
	limit := envInt("PREWARM_TOP_N", 10)
	if limit <= 0 {
		return
	}
	playlistManager.mu.RLock()
	candidates := make([]MusicItem, 0, 64)
	for _, pls := range playlistManager.UserPlaylists {
		for _, pl := range pls {
			candidates = append(candidates, pl.Songs...)
		}
	}
	playlistManager.mu.RUnlock()

	seen := map[string]struct{}{}
	done := 0
	for _, it := range candidates {
		if done >= limit {
			break
		}
		title := strings.TrimSpace(it.Title)
		artist := strings.TrimSpace(it.Artist)
		if title == "" {
			continue
		}
		key := strings.ToLower(artist + "|" + title)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		cacheFile := musicCacheFile(title, artist)
		if _, err := os.Stat(cacheFile); err == nil {
			continue // 已经有缓存
		}
		// 有人在听就先别转：这台 N1 只有 4 核，转码一上就把播放拖成一卡一卡。
		// 宁可少预热几首，也不能让正在放的歌卡。
		if audioBusy() {
			fmt.Printf("[Prewarm] 检测到正在播放，跳过本轮预热（已转 %d 首）\n", done)
			break
		}
		t0 := time.Now()
		remoteURL := getRemoteMusicURLOnly(title, artist)
		if remoteURL == "" {
			continue
		}
		if err := ensureMusicCache(remoteURL, cacheFile); err != nil {
			fmt.Printf("[Prewarm] ❌ %s - %s: %v\n", artist, title, err)
			continue
		}
		done++
		fmt.Printf("[Prewarm] ✅ %s - %s（%.1fs）\n", artist, title, time.Since(t0).Seconds())
		// 一首一首来，中间让出 CPU：预热是后台任务，不能和正在播放的
		// 设备抢资源（N1 这种 4 核小机器上，连续满载会让播放一卡一卡）。
		time.Sleep(3 * time.Second)
	}

	prewarmKnown.Lock()
	prewarmKnown.n = done
	prewarmKnown.Unlock()
	fmt.Printf("[Prewarm] 本轮完成，新转码 %d 首\n", done)
}

// nextDailyAt 返回下一个 hh:00（本地时区）。
func nextDailyAt(hour int) time.Time {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// startMaintenance 启动每天一次的备份与预热任务。
func startMaintenance() {
	if envFlag("BACKUP_ENABLED", true) {
		go func() {
			// 启动后 10 分钟补一次：NAS 常年在凌晨关机的话，光靠 03:00 定时
			// 可能一次都跑不上，这里按「上次备份超过 26 小时或从未备份」兜底。
			time.Sleep(10 * time.Minute)
			if backupNeeded() {
				fmt.Println("[Backup] 距上次备份已久，启动后补一次")
				if _, err := runBackup(); err != nil {
					fmt.Printf("[Backup] ❌ 补备份失败：%v\n", err)
				}
			}

			hour := envInt("BACKUP_HOUR", 3)
			for {
				time.Sleep(time.Until(nextDailyAt(hour)))
				if _, err := runBackup(); err != nil {
					backupState.Lock()
					backupState.LastErr = err.Error()
					backupState.Unlock()
					fmt.Printf("[Backup] ❌ %v\n", err)
				}
			}
		}()
		fmt.Printf("[Backup] 每天 %02d:00 自动备份到 %s，保留 %d 份\n",
			envInt("BACKUP_HOUR", 3), backupDir, envInt("BACKUP_KEEP", 7))
	}

	if envInt("PREWARM_TOP_N", 10) > 0 {
		go func() {
			// 起来别马上抢 CPU，等十分钟让服务先稳定。
			// 原来只等 3 分钟，部署完正好撞上用户试听，转码把播放拖成一卡一卡。
			time.Sleep(10 * time.Minute)
			warmFavorites()
			warmPlaylistLyrics(envInt("PREWARM_LYRIC_N", 15))
			hour := envInt("PREWARM_HOUR", 4)
			for {
				time.Sleep(time.Until(nextDailyAt(hour)))
				warmFavorites()
				warmPlaylistLyrics(envInt("PREWARM_LYRIC_N", 15))
			}
		}()
	}
}

// backupNeeded 判断是否需要补一次备份：没有备份，或最近一份超过 26 小时。
func backupNeeded() bool {
	list := listBackups()
	if len(list) == 0 {
		return true
	}
	// At 只有到分钟，用文件 mtime 更准
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return false
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		if info, err := e.Info(); err == nil {
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	return time.Since(newest) > 26*time.Hour
}
