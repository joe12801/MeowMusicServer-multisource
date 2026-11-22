package main

// 运行期设置（目前只有一项：外部音源 mp3api 的 KEY）
//
// 为什么要有这个东西：
//   MUSIC_MP3API_KEY 原本只能写进 .env 然后重启容器。用户没有 SSH 习惯时
//   就等于这个源永远关着——搜索「Beyond 长城」全是 DJ 版，却看不出原因。
//   现在可以在 /tools 页面直接填 KEY，即时生效（mp3ApiKey() 每次请求现读），
//   不用重建镜像、不用重启进程。
//
// 取值优先级：环境变量 > 页面保存的值。
// 环境变量是「部署时定死」的，优先级更高，避免页面上的值把运维配置顶掉。
//
// KEY 落盘在 files/settings.json（0600），日志里只出现打码后的片段。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const appSettingsFile = "./files/settings.json"

type appSettings struct {
	MP3ApiKey  string `json:"mp3api_key,omitempty"`
	LrcapiBase string `json:"lrcapi_base,omitempty"`
	LrcapiAuth string `json:"lrcapi_auth,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	UpdatedBy  string `json:"updated_by,omitempty"`
}

var (
	settingsMu   sync.RWMutex
	settingsData appSettings
)

func initSettings() {
	b, err := os.ReadFile(appSettingsFile)
	if err != nil {
		return
	}
	var s appSettings
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Printf("[Settings] 配置文件损坏，已忽略：%v\n", err)
		return
	}
	settingsMu.Lock()
	settingsData = s
	settingsMu.Unlock()
	if s.MP3ApiKey != "" {
		fmt.Printf("[Settings] 已载入外部音源 KEY：%s\n", maskSecret(s.MP3ApiKey))
	}
}

func saveSettingsLocked() error {
	b, err := json.MarshalIndent(settingsData, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(appSettingsFile, b, 0o600)
}

// storedMP3ApiKey 页面里保存的 KEY（没有则空）
func storedMP3ApiKey() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return strings.TrimSpace(settingsData.MP3ApiKey)
}

// storedLrcapiBase / storedLrcapiAuth 页面里保存的歌词源配置
func storedLrcapiBase() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return strings.TrimSpace(settingsData.LrcapiBase)
}

func storedLrcapiAuth() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return strings.TrimSpace(settingsData.LrcapiAuth)
}

// maskSecret 打码：只留头 4 位和尾 4 位
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 6) + s[len(s)-4:]
}

// extSettingsSnapshot 给前端看的状态（绝不返回明文 KEY）
func extSettingsSnapshot() map[string]interface{} {
	envKey := strings.TrimSpace(os.Getenv("MUSIC_MP3API_KEY"))
	fileKey := storedMP3ApiKey()

	from := "none"
	key := ""
	switch {
	case envKey != "":
		from = "env"
		key = envKey
	case fileKey != "":
		from = "file"
		key = fileKey
	}
	return map[string]interface{}{
		"configured": key != "",
		"from":       from,
		"masked":     maskSecret(key),
		"enabled":    srcEnabled("mp3api"),
		"first":      mp3ApiFirst(),
		"base":       mp3ApiBase(),
	}
}

// HandleExtSettings 读/写外部音源配置。
//   GET  -> 状态（KEY 打码）
//   POST {"mp3api_key":"..."} -> 保存（空字符串=清除）
func HandleExtSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(extSettingsSnapshot())
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"success":false,"message":"只支持 GET / POST"}`, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		MP3ApiKey string `json:"mp3api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, `{"success":false,"message":"请求格式不对"}`, http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(req.MP3ApiKey)
	if len(key) > 256 {
		http.Error(w, `{"success":false,"message":"KEY 太长"}`, http.StatusBadRequest)
		return
	}
	// KEY 只允许可见 ASCII，避免粘进来不可见字符后一直 401
	for _, c := range key {
		if c < 0x21 || c > 0x7e {
			http.Error(w, `{"success":false,"message":"KEY 里有空格或不可见字符，请重新复制"}`, http.StatusBadRequest)
			return
		}
	}

	settingsMu.Lock()
	settingsData.MP3ApiKey = key
	settingsData.UpdatedAt = time.Now().Unix()
	settingsData.UpdatedBy = resolveUserID(r)
	err := saveSettingsLocked()
	settingsMu.Unlock()
	if err != nil {
		fmt.Printf("[Settings] 保存失败：%v\n", err)
		http.Error(w, `{"success":false,"message":"写入配置文件失败"}`, http.StatusInternalServerError)
		return
	}
	if key == "" {
		fmt.Printf("[Settings] 外部音源 KEY 已清除\n")
	} else {
		fmt.Printf("[Settings] 外部音源 KEY 已更新：%s\n", maskSecret(key))
	}

	out := extSettingsSnapshot()
	out["success"] = true
	json.NewEncoder(w).Encode(out)
}

// HandleExtSettingsTest 用当前 KEY 真打一次 mp3api 的搜索接口，告诉用户到底通不通。
// 只取 1 条、不等音频，1~3 秒返回。
func HandleExtSettingsTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, `{"success":false,"message":"只支持 POST"}`, http.StatusMethodNotAllowed)
		return
	}
	key := mp3ApiKey()
	if key == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "status": 0, "message": "还没有配置 KEY",
		})
		return
	}

	target := fmt.Sprintf("%s/api/search?q=%s&limit=1&api_key=%s",
		mp3ApiBase(), url.QueryEscape("周杰伦 晴天"), url.QueryEscape(key))
	client := &http.Client{Timeout: 20 * time.Second}
	started := time.Now()
	resp, err := client.Get(target)
	if err != nil {
		fmt.Printf("[Settings] 测试外部音源失败：%v\n", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "status": 0, "message": "连不上 " + mp3ApiBase() + "：" + err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	elapsed := int(time.Since(started).Milliseconds())

	msg := ""
	ok := false
	switch {
	case resp.StatusCode == 200:
		ok = true
		msg = "连接正常，外部源可用"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		msg = fmt.Sprintf("KEY 无效或已过期（服务端返回 %d）", resp.StatusCode)
	default:
		msg = fmt.Sprintf("服务端返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": ok,
		"status":  resp.StatusCode,
		"ms":      elapsed,
		"message": msg,
	})
}

// ---------- 歌词源 LrcAPI ----------

func lyricSettingsSnapshot(r *http.Request) map[string]interface{} {
	envBase := strings.TrimSpace(os.Getenv("LRCAPI_BASE"))
	envAuth := strings.TrimSpace(os.Getenv("LRCAPI_AUTH"))

	from := "default"
	switch {
	case envBase != "":
		from = "env"
	case storedLrcapiBase() != "":
		from = "file"
	}
	authFrom := "none"
	switch {
	case envAuth != "":
		authFrom = "env"
	case storedLrcapiAuth() != "":
		authFrom = "file"
	}

	return map[string]interface{}{
		"base":            lrcapiBase(),
		"from":            from,
		"default_base":    lrcapiDefaultBase,
		"enabled":         lrcapiEnabled(),
		"auth_configured": lrcapiAuth() != "",
		"auth_masked":     maskSecret(lrcapiAuth()),
		"auth_from":       authFrom,
		"can_edit":        settingsOwnerOK(r),
	}
}

// HandleLyricSettings 读/写歌词源配置。
//   GET  -> 当前地址、来源、是否可用
//   POST {"base":"http://lrcapi:28883","auth":"..."} -> 保存（base 空=恢复默认，auth 空=清除）
func HandleLyricSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(lyricSettingsSnapshot(r))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"success":false,"message":"只支持 GET / POST"}`, http.StatusMethodNotAllowed)
		return
	}
	// base 决定服务器会去访问哪台主机，等于一个内向的出口，只给站长改
	if !settingsOwnerOK(r) {
		http.Error(w, `{"success":false,"message":"只有站长（最早注册的账号）能改这里"}`, http.StatusForbidden)
		return
	}

	var req struct {
		Base string `json:"base"`
		Auth string `json:"auth"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, `{"success":false,"message":"请求格式不对"}`, http.StatusBadRequest)
		return
	}

	base, err := normalizeLrcapiBase(req.Base)
	if err != nil {
		http.Error(w, `{"success":false,"message":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	auth := strings.TrimSpace(req.Auth)
	if len(auth) > 256 {
		http.Error(w, `{"success":false,"message":"鉴权 KEY 太长"}`, http.StatusBadRequest)
		return
	}

	settingsMu.Lock()
	settingsData.LrcapiBase = base
	settingsData.LrcapiAuth = auth
	settingsData.UpdatedAt = time.Now().Unix()
	settingsData.UpdatedBy = resolveUserID(r)
	err = saveSettingsLocked()
	settingsMu.Unlock()
	if err != nil {
		fmt.Printf("[Settings] 歌词源配置保存失败：%v\n", err)
		http.Error(w, `{"success":false,"message":"写入配置文件失败"}`, http.StatusInternalServerError)
		return
	}
	fmt.Printf("[Settings] 歌词源已更新：%s（鉴权 %s）\n", lrcapiBase(), map[bool]string{true: maskSecret(auth), false: "无"}[auth != ""])

	out := lyricSettingsSnapshot(r)
	out["success"] = true
	json.NewEncoder(w).Encode(out)
}

// HandleLyricSettingsTest 用当前配置真打一次歌词接口，顺手校验「不会张冠李戴」。
func HandleLyricSettingsTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, `{"success":false,"message":"只支持 POST"}`, http.StatusMethodNotAllowed)
		return
	}

	started := time.Now()
	res, err := lyricFromLrcAPI("晴天", "周杰伦")
	ms := int(time.Since(started).Milliseconds())
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"ms":      ms,
			"base":    lrcapiBase(),
			"message": "连不上或没结果：" + err.Error(),
		})
		return
	}

	lines := len(strings.Split(strings.TrimSpace(res.LRC+res.Plain), "\n"))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"ms":      ms,
		"base":    lrcapiBase(),
		"synced":  res.Synced,
		"lines":   lines,
		"sample":  firstLyricLine(res.LRC),
		"message": fmt.Sprintf("连接正常，命中《晴天》共 %d 行歌词", lines),
	})
}

func firstLyricLine(lrc string) string {
	for _, ln := range strings.Split(lrc, "\n") {
		t := strings.TrimSpace(lrcTagRe.ReplaceAllString(ln, ""))
		if t != "" {
			return t
		}
	}
	return ""
}
