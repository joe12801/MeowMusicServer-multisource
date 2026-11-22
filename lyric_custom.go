package main

// 歌词手动纠错
//
// 自动源（LRCLIB / 网易云）难免出问题：搜到同名的另一首歌、时间轴整体偏移、
// 或者干脆只有纯文本没时间轴。以前遇到这种情况只能干看着。
//
// 现在允许登录用户覆盖任意一首歌的歌词：
//   POST   /api/lyric/custom   {song, singer, lrc}  保存（带时间轴=逐行跟唱，纯文本=静态滚动）
//   DELETE /api/lyric/custom?song=&singer=          删除覆盖，回到自动源
//
// 覆盖存在 files/custom_lyrics.json，键是「歌手|歌名」规范化后的字符串。
// /api/lyric 命中时直接返回，source 字段为 custom，前端据此把按钮标成「已自定义」。
//
// 为什么不做成「每人一份」：这是家用单机 NAS，全家共用一个曲库，
// 歌词对错是客观事实而不是个人偏好，共享一份最省事也最不容易互相打架。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	customLyricFile = "./files/custom_lyrics.json"
	customLyricMax  = 64 << 10 // 单首歌词上限 64KB，正常 LRC 只有几 KB
)

type customLyric struct {
	LRC   string `json:"lrc,omitempty"`
	Plain string `json:"plain,omitempty"`
	By    string `json:"by,omitempty"`
	At    int64  `json:"at,omitempty"`
}

var (
	customLyricMu sync.RWMutex
	customLyrics  = map[string]customLyric{}
)

// 时间标签：[01:23.45] / [01:23] / [01:23:450]，兼容中英文全角括号
var lyricTimeTagRe = regexp.MustCompile(`\[\s*\d{1,3}\s*[:：]\s*\d{1,2}(?:[.:：]\d{1,3})?\s*\]`)

// customLyricKey 把「歌手 + 歌名」压成稳定的键：
// 小写、去首尾空格、内部连续空白折成一个空格，去掉会破坏 JSON 可读性的竖线。
func customLyricKey(song, singer string) string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.ReplaceAll(s, "\r", " ")
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.Join(strings.Fields(s), " ")
		return strings.TrimSpace(strings.ReplaceAll(s, "|", "／"))
	}
	return norm(singer) + "|" + norm(song)
}

// initCustomLyrics 启动时把覆盖读进内存，读不到就静默跳过（不影响主流程）。
func initCustomLyrics() {
	b, err := os.ReadFile(customLyricFile)
	if err != nil {
		return
	}
	var m map[string]customLyric
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Printf("[Lyric] 自定义歌词文件损坏，已忽略：%v\n", err)
		return
	}
	customLyricMu.Lock()
	customLyrics = m
	customLyricMu.Unlock()
	fmt.Printf("[Lyric] 已载入 %d 首自定义歌词\n", len(m))
}

func saveCustomLyrics() error {
	customLyricMu.RLock()
	b, err := json.MarshalIndent(customLyrics, "", "  ")
	customLyricMu.RUnlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(customLyricFile, b, 0o644)
}

// lookupCustomLyric 命中人工歌词时返回结果，否则返回 nil（调用方回落自动源）。
func lookupCustomLyric(song, singer string) *lyricResult {
	key := customLyricKey(song, singer)
	customLyricMu.RLock()
	item, ok := customLyrics[key]
	customLyricMu.RUnlock()
	if !ok {
		return nil
	}
	lrc := strings.TrimSpace(item.LRC)
	plain := strings.TrimSpace(item.Plain)
	if lrc == "" && plain == "" {
		return nil
	}
	return &lyricResult{
		LRC:        item.LRC,
		Plain:      item.Plain,
		Synced:     lrc != "",
		Source:     "custom",
		TransTried: true, // 人工歌词不再去补翻译，避免和时间轴错位
	}
}

func customLyricCount() int {
	customLyricMu.RLock()
	defer customLyricMu.RUnlock()
	return len(customLyrics)
}

// HandleCustomLyric 处理保存（POST）与删除（DELETE）。
func HandleCustomLyric(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	userID := resolveUserID(r)

	switch r.Method {
	case http.MethodPost:
		var req struct {
			Song   string `json:"song"`
			Singer string `json:"singer"`
			LRC    string `json:"lrc"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, customLyricMax)).Decode(&req); err != nil {
			http.Error(w, `{"success":false,"message":"歌词内容过大或格式不对"}`, http.StatusBadRequest)
			return
		}
		req.Song = strings.TrimSpace(req.Song)
		if req.Song == "" {
			http.Error(w, `{"success":false,"message":"缺少歌名"}`, http.StatusBadRequest)
			return
		}
		text := strings.TrimSpace(req.LRC)
		if text == "" {
			http.Error(w, `{"success":false,"message":"歌词不能为空（想删掉请点「恢复自动」）"}`, http.StatusBadRequest)
			return
		}
		if len(text) > customLyricMax {
			http.Error(w, `{"success":false,"message":"歌词超过 64KB"}`, http.StatusBadRequest)
			return
		}

		item := customLyric{By: userID, At: time.Now().Unix()}
		if lyricTimeTagRe.MatchString(text) {
			item.LRC = text
		} else {
			// 没有时间标签就当纯文本，前端静态滚动显示
			item.Plain = text
		}

		key := customLyricKey(req.Song, req.Singer)
		customLyricMu.Lock()
		customLyrics[key] = item
		customLyricMu.Unlock()
		if err := saveCustomLyrics(); err != nil {
			fmt.Printf("[Lyric] 保存自定义歌词失败：%v\n", err)
		}
		fmt.Printf("[Lyric] %s 覆盖歌词《%s - %s》：%d 字，带时间轴=%v\n",
			userID, req.Singer, req.Song, len(text), item.LRC != "")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"synced":  item.LRC != "",
			"source":  "custom",
			"lines":   len(strings.Split(text, "\n")),
		})
		return

	case http.MethodDelete:
		song := strings.TrimSpace(r.URL.Query().Get("song"))
		singer := r.URL.Query().Get("singer")
		if song == "" {
			http.Error(w, `{"success":false,"message":"缺少歌名"}`, http.StatusBadRequest)
			return
		}
		key := customLyricKey(song, singer)
		customLyricMu.Lock()
		_, existed := customLyrics[key]
		delete(customLyrics, key)
		customLyricMu.Unlock()
		if existed {
			if err := saveCustomLyrics(); err != nil {
				fmt.Printf("[Lyric] 删除自定义歌词失败：%v\n", err)
			}
			fmt.Printf("[Lyric] %s 恢复自动歌词《%s - %s》\n", userID, singer, song)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "removed": existed})
		return

	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, `{"success":false,"message":"只支持 POST / DELETE"}`, http.StatusMethodNotAllowed)
	}
}
