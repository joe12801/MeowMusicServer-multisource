package main

// 网易云歌单「每天自动增量同步」
//
// 导入歌单只搬一次：源歌单后面又加了 20 首，本地还是老样子，得再手动导一次
// （还会整单重拉）。这里把「导入」升级成一条可长期维护的同步关系：
//
//   - 导入时勾上「每天自动同步」，就登记一条 job（源歌单 ID + 本地歌单 ID）；
//   - 每天 SYNC_HOUR（默认 05:00）后台拉一次源歌单，只把新歌追加到本地歌单
//     （AddSongsToUserPlaylist 自带去重，老歌原样不动、顺序不变）；
//   - 开机 5 分钟后补一次：NAS 不是 7×24 开着的话，光靠定点任务可能错过；
//   - 每条 job 记最近一次结果（新增几首 / 失败原因），/tools 页面能看能手动触发。
//
// 只做「加歌」，不删歌：源歌单里删掉的歌本地仍保留——避免误删用户自己
// 又加回来的内容，也让操作完全可预期。
//
// 接口：
//   GET  /api/user/playlists/sync-jobs                 列出我的同步任务
//   POST /api/user/playlists/sync-run     {job_id?}     立即同步（不传=全部）
//   POST /api/user/playlists/sync-toggle  {job_id,enabled}
//   POST /api/user/playlists/sync-delete  {job_id}

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	syncFilePath = "./files/sync.json"
	syncMinGap   = 20 * time.Hour // 补跑判定：距上次同步不足这么久就先跳过
)

type syncJob struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	PlaylistID string `json:"playlist_id"`
	NeteaseID  string `json:"netease_id"`
	SourceName string `json:"source_name"`
	Enabled    bool   `json:"enabled"`

	Added     int    `json:"added"`      // 累计从源歌单搬过来的歌
	Total     int    `json:"total"`      // 源歌单总数
	LastAdded int    `json:"last_added"` // 上次同步新增
	LastAt    int64  `json:"last_at"`
	LastErr   string `json:"last_err"`
	CreatedAt int64  `json:"created_at"`
}

type syncStore struct {
	mu       sync.Mutex
	Jobs     []*syncJob `json:"jobs"`
	running  bool
	lastSave time.Time
	dirty    bool
}

var syncJobs = &syncStore{}

func initSyncStore() {
	data, err := os.ReadFile(syncFilePath)
	if err != nil {
		return
	}
	var loaded syncStore
	if json.Unmarshal(data, &loaded) != nil || loaded.Jobs == nil {
		return
	}
	syncJobs.Jobs = loaded.Jobs
}

func (s *syncStore) saveLocked(force bool) {
	if !s.dirty {
		return
	}
	if !force && time.Since(s.lastSave) < 10*time.Second {
		return
	}
	data, err := json.MarshalIndent(struct {
		Jobs []*syncJob `json:"jobs"`
	}{s.Jobs}, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(syncFilePath, data, 0o600); err != nil {
		fmtLogf("写 sync.json 失败: %v", err)
		return
	}
	s.lastSave = time.Now()
	s.dirty = false
}

// registerSyncJob 登记（或复用）一条同步关系。
func registerSyncJob(userID, playlistID, neteaseID, sourceName string) *syncJob {
	syncJobs.mu.Lock()
	defer syncJobs.mu.Unlock()

	for _, j := range syncJobs.Jobs {
		if j.UserID == userID && j.PlaylistID == playlistID {
			j.NeteaseID = neteaseID
			j.SourceName = sourceName
			j.Enabled = true
			j.LastErr = ""
			syncJobs.dirty = true
			syncJobs.saveLocked(true)
			return j
		}
	}

	j := &syncJob{
		ID:         generateID(),
		UserID:     userID,
		PlaylistID: playlistID,
		NeteaseID:  neteaseID,
		SourceName: sourceName,
		Enabled:    true,
		LastAt:     time.Now().Unix(),
		CreatedAt:  time.Now().Unix(),
	}
	syncJobs.Jobs = append(syncJobs.Jobs, j)
	syncJobs.dirty = true
	syncJobs.saveLocked(true)
	return j
}

// removeJobsForPlaylist 歌单删掉后，同步关系一起清掉，免得每天报错。
func removeJobsForPlaylist(userID, playlistID string) {
	syncJobs.mu.Lock()
	defer syncJobs.mu.Unlock()
	kept := syncJobs.Jobs[:0]
	for _, j := range syncJobs.Jobs {
		if j.UserID == userID && j.PlaylistID == playlistID {
			continue
		}
		kept = append(kept, j)
	}
	syncJobs.Jobs = kept
	syncJobs.dirty = true
	syncJobs.saveLocked(true)
}

// runSyncJob 真正跑一次同步：拉源歌单 → 批量追加 → 记结果。
func runSyncJob(j *syncJob) (int, int, error) {
	limit := envInt("SYNC_MAX_SONGS", 500)
	if limit <= 0 {
		limit = 500
	}

	if _, err := playlistManager.GetUserPlaylistByID(j.UserID, j.PlaylistID); err != nil {
		j.LastErr = "本地歌单已不存在"
		j.Enabled = false
		return 0, 0, fmt.Errorf("本地歌单已不存在")
	}

	name, songs, total, err := fetchNeteasePlaylist(j.NeteaseID, limit)
	now := time.Now().Unix()

	syncJobs.mu.Lock()
	j.LastAt = now
	if err != nil {
		j.LastErr = err.Error()
		j.LastAdded = 0
		syncJobs.dirty = true
		syncJobs.saveLocked(true)
		syncJobs.mu.Unlock()
		fmt.Printf("[Sync] ❌ 歌单 %s（%s）：%v\n", j.SourceName, j.NeteaseID, err)
		return 0, 0, err
	}

	added, skipped, aerr := playlistManager.AddSongsToUserPlaylist(j.UserID, j.PlaylistID, songs)
	if aerr != nil {
		j.LastErr = aerr.Error()
		j.LastAdded = 0
		syncJobs.dirty = true
		syncJobs.saveLocked(true)
		syncJobs.mu.Unlock()
		fmt.Printf("[Sync] ❌ 写入歌单失败：%v\n", aerr)
		return 0, 0, aerr
	}

	j.SourceName = name
	j.Total = total
	j.LastAdded = added
	j.Added += added
	j.LastErr = ""
	syncJobs.dirty = true
	syncJobs.saveLocked(true)
	syncJobs.mu.Unlock()

	fmt.Printf("[Sync] ✅ %q：新增 %d 首，跳过已有 %d 首（源共 %d 首）\n",
		name, added, skipped, total)
	return added, skipped, nil
}

// ---------- 调度 ----------

// runDueSyncs 把所有到期（或全部）的 job 串行跑一遍。
func runDueSyncs(catchUp bool) {
	syncJobs.mu.Lock()
	list := make([]*syncJob, 0, len(syncJobs.Jobs))
	for _, j := range syncJobs.Jobs {
		if j.Enabled {
			list = append(list, j)
		}
	}
	if syncJobs.running {
		syncJobs.mu.Unlock()
		return
	}
	syncJobs.running = true
	syncJobs.mu.Unlock()

	defer func() {
		syncJobs.mu.Lock()
		syncJobs.running = false
		syncJobs.mu.Unlock()
	}()

	for _, j := range list {
		if catchUp && time.Since(time.Unix(j.LastAt, 0)) < syncMinGap {
			continue
		}
		if _, _, err := runSyncJob(j); err != nil {
			// 单个失败不影响其它歌单
			time.Sleep(2 * time.Second)
			continue
		}
		// 顺序拉，别把网易云接口打急
		time.Sleep(3 * time.Second)
	}
}

// startSyncScheduler 每天定点同步 + 开机补跑。
func startSyncScheduler() {
	if !envFlag("SYNC_ENABLED", true) {
		fmt.Println("[Sync] 歌单自动同步已关闭（SYNC_ENABLED=0）")
		return
	}
	hour := envInt("SYNC_HOUR", 5)
	go func() {
		time.Sleep(5 * time.Minute)
		runDueSyncs(true)
		for {
			time.Sleep(time.Until(nextDailyAt(hour)))
			runDueSyncs(false)
		}
	}()
	fmt.Printf("[Sync] 每天 %02d:00 自动同步网易云歌单新增曲目\n", hour)
}

// ---------- HTTP ----------

func syncJobView(j *syncJob) map[string]interface{} {
	out := map[string]interface{}{
		"job_id":      j.ID,
		"playlist_id": j.PlaylistID,
		"netease_id":  j.NeteaseID,
		"source_name": j.SourceName,
		"enabled":     j.Enabled,
		"added":       j.Added,
		"total":       j.Total,
		"last_added":  j.LastAdded,
		"last_err":    j.LastErr,
	}
	if j.LastAt > 0 {
		out["last_at"] = time.Unix(j.LastAt, 0).Format("2006-01-02 15:04")
	}
	if pl, err := playlistManager.GetUserPlaylistByID(j.UserID, j.PlaylistID); err == nil {
		out["playlist_name"] = pl.Name
		out["songs"] = len(pl.Songs)
	}
	return out
}

// HandleSyncJobs 列出当前用户的同步任务。
func HandleSyncJobs(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	syncJobs.mu.Lock()
	list := make([]*syncJob, 0, len(syncJobs.Jobs))
	for _, j := range syncJobs.Jobs {
		if j.UserID == userID {
			list = append(list, j)
		}
	}
	syncJobs.mu.Unlock()

	out := make([]map[string]interface{}, 0, len(list))
	for _, j := range list {
		out = append(out, syncJobView(j))
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jobs":       out,
		"hour":       envInt("SYNC_HOUR", 5),
		"enabled":    envFlag("SYNC_ENABLED", true),
		"max_songs":  envInt("SYNC_MAX_SONGS", 500),
	})
}

// HandleSyncRun 立即同步一次（不传 job_id 就同步我的全部任务）。
func HandleSyncRun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持 POST"})
		return
	}
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未登录"})
		return
	}

	var req struct {
		JobID string `json:"job_id"`
	}
	body, _ := readLimitedBody(r, 4<<10)
	if len(body) > 0 {
		json.Unmarshal(body, &req)
	}

	syncJobs.mu.Lock()
	targets := make([]*syncJob, 0, len(syncJobs.Jobs))
	for _, j := range syncJobs.Jobs {
		if j.UserID != userID {
			continue
		}
		if req.JobID != "" && j.ID != req.JobID {
			continue
		}
		targets = append(targets, j)
	}
	syncJobs.mu.Unlock()

	if len(targets) == 0 {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "没有找到同步任务"})
		return
	}

	type result struct {
		JobID     string `json:"job_id"`
		SourceName string `json:"source_name"`
		Added     int    `json:"added"`
		Skipped   int    `json:"skipped"`
		Error     string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(targets))
	totalAdded := 0
	for _, j := range targets {
		added, skipped, err := runSyncJob(j)
		res := result{JobID: j.ID, SourceName: j.SourceName, Added: added, Skipped: skipped}
		if err != nil {
			res.Error = err.Error()
		} else {
			totalAdded += added
		}
		results = append(results, res)
		time.Sleep(1 * time.Second)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"added":   totalAdded,
		"results": results,
	})
}

// HandleSyncToggle 暂停 / 恢复一条同步任务。
func HandleSyncToggle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		JobID   string `json:"job_id"`
		Enabled *bool  `json:"enabled"`
	}
	body, _ := readLimitedBody(r, 4<<10)
	if err := json.Unmarshal(body, &req); err != nil || req.JobID == "" || req.Enabled == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "参数错误"})
		return
	}

	ok := false
	syncJobs.mu.Lock()
	for _, j := range syncJobs.Jobs {
		if j.ID == req.JobID && j.UserID == userID {
			j.Enabled = *req.Enabled
			if j.Enabled {
				j.LastErr = ""
			}
			ok = true
			break
		}
	}
	syncJobs.dirty = true
	syncJobs.saveLocked(true)
	syncJobs.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "任务不存在"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleSyncDelete 取消一条同步任务（不影响已经导入的歌）。
func HandleSyncDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		JobID string `json:"job_id"`
	}
	body, _ := readLimitedBody(r, 4<<10)
	if err := json.Unmarshal(body, &req); err != nil || req.JobID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "参数错误"})
		return
	}

	syncJobs.mu.Lock()
	kept := syncJobs.Jobs[:0]
	found := false
	for _, j := range syncJobs.Jobs {
		if j.ID == req.JobID && j.UserID == userID {
			found = true
			continue
		}
		kept = append(kept, j)
	}
	syncJobs.Jobs = kept
	syncJobs.dirty = true
	syncJobs.saveLocked(true)
	syncJobs.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{"success": found})
}

// syncSnapshot 给 /status 用。
func syncSnapshot() map[string]interface{} {
	syncJobs.mu.Lock()
	defer syncJobs.mu.Unlock()
	enabled, failed := 0, 0
	var lastAt int64
	for _, j := range syncJobs.Jobs {
		if j.Enabled {
			enabled++
		}
		if j.LastErr != "" {
			failed++
		}
		if j.LastAt > lastAt {
			lastAt = j.LastAt
		}
	}
	out := map[string]interface{}{
		"jobs":    len(syncJobs.Jobs),
		"enabled": enabled,
		"failed":  failed,
		"hour":    envInt("SYNC_HOUR", 5),
		"on":      envFlag("SYNC_ENABLED", true),
		"running": syncJobs.running,
	}
	if lastAt > 0 {
		out["last_at"] = time.Unix(lastAt, 0).Format("2006-01-02 15:04")
	}
	return out
}

// readLimitedBody 读请求体（带上限），避免 sync 接口被灌爆。
func readLimitedBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, max))
}
