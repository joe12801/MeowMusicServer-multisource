package main

// 音源健康面板
//
// 起因：这台机器在境外，酷我经常直接回 407（地域限制），网易云时好时坏，
// 而排障只能翻 docker logs 一条条 grep。这里把每个音源的
// 成功/失败次数、平均耗时、最后一次结果记在内存里，
// 由 /api/sources 暴露给 /status 页面看。
//
// 只在内存里，重启清零——它描述的是「这次进程运行期间各源表现如何」，
// 不做持久化，也就不需要考虑落盘失败、并发写坏文件这些事。

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var startedAt = time.Now()

type sourceHealth struct {
	Name        string  `json:"name"`
	Group       string  `json:"group"`
	Enabled     bool    `json:"enabled"`
	Success     int64   `json:"success"`
	Fail        int64   `json:"fail"`
	ConsecFail  int     `json:"consecutive_fail"`
	AvgMs       float64 `json:"avg_ms"`
	LastMs      float64 `json:"last_ms"`
	LastOK      string  `json:"last_ok,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
	LastAttempt string  `json:"last_attempt,omitempty"`
	CoolingDown bool    `json:"cooling_down"`
	CooldownEnd string  `json:"cooldown_until,omitempty"`

	sumMs float64
	// until 非零表示该源进入冷却：这段时间内直接跳过，不再发请求
	until time.Time
}

var (
	sourceHealthMu sync.Mutex
	sourceHealthDB = map[string]*sourceHealth{}
	sourceOrder    []string
)

// baseSourceName 把 "kuwo-strict" / "nettease-ncm" 这类带阶段后缀的名字
// 归到 MUSIC_SOURCES 里的开关名（kuwo / netease …）。
func baseSourceName(name string) string {
	if i := strings.IndexByte(name, '-'); i > 0 {
		return name[:i]
	}
	return name
}

// 自动降级参数：
//   SOURCE_DEGRADE_AFTER=5   连续失败多少次进冷却（0 = 关闭自动降级）
//   SOURCE_COOLDOWN_MIN=5    冷却多少分钟，到点自动放行再试一次
func degradeAfter() int {
	n := envInt("SOURCE_DEGRADE_AFTER", 5)
	if n < 0 {
		return 0
	}
	return n
}

func degradeCooldown() time.Duration {
	m := envInt("SOURCE_COOLDOWN_MIN", 5)
	if m < 1 {
		m = 1
	}
	return time.Duration(m) * time.Minute
}

// sourceCoolingDown 判断某源是否在冷却期内；冷却刚到期就顺手清标记，
// 让下一首歌重新试一次（试失败会立刻再进冷却，不会反复拖慢）。
func sourceCoolingDown(name string) bool {
	sourceHealthMu.Lock()
	defer sourceHealthMu.Unlock()

	h, ok := sourceHealthDB[name]
	if !ok || h.until.IsZero() {
		return false
	}
	if time.Now().Before(h.until) {
		return true
	}
	h.until = time.Time{}
	h.CoolingDown = false
	h.CooldownEnd = ""
	h.ConsecFail = 0
	return false
}

func recordSourceResult(name string, ok bool, d time.Duration, detail string) {
	now := time.Now()
	ms := float64(d.Microseconds()) / 1000.0

	sourceHealthMu.Lock()
	defer sourceHealthMu.Unlock()

	h, exists := sourceHealthDB[name]
	if !exists {
		h = &sourceHealth{Name: name, Group: baseSourceName(name)}
		sourceHealthDB[name] = h
		sourceOrder = append(sourceOrder, name)
	}
	h.LastMs = ms
	h.LastAttempt = now.Format("15:04:05")
	if ok {
		h.Success++
		h.ConsecFail = 0
		h.sumMs += ms
		h.AvgMs = h.sumMs / float64(h.Success)
		h.LastOK = now.Format("15:04:05")
		if !h.until.IsZero() {
			fmtLogf("音源 %s 恢复正常，解除冷却", name)
			h.until = time.Time{}
			h.CoolingDown = false
			h.CooldownEnd = ""
		}
	} else {
		h.Fail++
		h.ConsecFail++
		if detail != "" {
			h.LastError = now.Format("15:04:05") + " " + trimForLog(detail, 120)
		}
		// 连续失败到阈值就进冷却：后面的歌直接跳过这个源，
		// 不再每次都白等一次超时（酷我 407、源站挂掉时最明显）。
		if after := degradeAfter(); after > 0 && h.ConsecFail >= after && h.until.IsZero() {
			h.until = now.Add(degradeCooldown())
			h.CoolingDown = true
			h.CooldownEnd = h.until.Format("15:04:05")
			fmtLogf("音源 %s 连续失败 %d 次，冷却到 %s", name, h.ConsecFail, h.CooldownEnd)
		}
	}
}

func trimForLog(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// trySource 包一层调用：记录耗时与命中情况，返回原结果。
// 冷却中的源直接返回空结果，不发请求——这是「自动降级」真正生效的地方，
// 调用点（helper.go 里十来个源）一行都不用改。
func trySource(name string, fn func() MusicItem) MusicItem {
	if sourceCoolingDown(name) {
		fmtLogf("音源 %s 冷却中，本次跳过", name)
		return MusicItem{}
	}
	t0 := time.Now()
	item := fn()
	ok := strings.TrimSpace(item.Title) != "" && strings.TrimSpace(item.AudioURL) != ""
	detail := ""
	if !ok {
		if item.Title != "" {
			detail = "有候选但无可播地址（多为版权/地域限制）"
		} else {
			detail = "无匹配候选"
		}
	}
	recordSourceResult(name, ok, time.Since(t0), detail)
	return item
}

func snapshotSourceHealth() []sourceHealth {
	sourceHealthMu.Lock()
	defer sourceHealthMu.Unlock()

	out := make([]sourceHealth, 0, len(sourceOrder))
	for _, name := range sourceOrder {
		h := *sourceHealthDB[name]
		h.Enabled = srcEnabled(h.Group)
		h.CoolingDown = !h.until.IsZero() && time.Now().Before(h.until)
		if !h.CoolingDown {
			h.CooldownEnd = ""
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// HandleSourcesHealth 返回音源/缓存/转码的整体状态，供 /status 页面轮询。
func HandleSourcesHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	resp := map[string]interface{}{
		"sources":        snapshotSourceHealth(),
		"cache":          cacheSnapshot(),
		"transcode":      transcodeStats(),
		"quota":          quotaSnapshot(),
		"degrade":        map[string]interface{}{"after": degradeAfter(), "cooldown_min": int(degradeCooldown().Minutes())},
		"backup":         backupStatus(),
		"history":        historyStats(),
		"sync":           syncSnapshot(),
		"guest":          guestSnapshot(),
		"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		"enabled_list":   strings.TrimSpace(getenvDefault("MUSIC_SOURCES", "（全部启用）")),
		"now":            time.Now().Format("2006-01-02 15:04:05"),
	}
	json.NewEncoder(w).Encode(resp)
}

func getenvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
