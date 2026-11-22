package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const (
	TAG = "MeowEmbeddedMusicServer"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		fmt.Printf("[Warning] %s Loading .env file failed: %v\nUse the default configuration instead.\n", TAG, err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		fmt.Printf("[Warning] %s PORT environment variable not set\nUse the default port 2233 instead.\n", TAG)
		port = "2233"
	}

	// Initialize user store
	InitUserStore()

	// Initialize playlist manager
	InitPlaylistManager()

	// Initialize device manager
	GetDeviceManager()

	// Initialize daily transcode quota (files/usage.json)
	initQuotaStore()

	// Initialize play history (files/history.json)
	initHistoryStore()

	// Initialize 播放统计（files/stats.json）、游客试听额度（files/guest.json）、
	// 网易云歌单自动同步（files/sync.json）
	initStatsStore()
	initGuestStore()
	initSyncStore()

	// 人工纠错的歌词（files/custom_lyrics.json），优先于自动源
	initCustomLyrics()

	// 运行期设置（files/settings.json）：外部音源 mp3api 的 KEY 可在页面里填
	initSettings()

	// Register handlers
	// handle() = 路由 + panic 恢复 + 安全响应头
	handle("/", indexHandler)
	handle("/stream_pcm", apiHandler)
	handle("/stream_live", streamLiveHandler) // 实时流式转码，边下载边播放
	handle("/api/search", HandleSearch) // Search API
	handle("/api/ext_audio", HandleExtAudio) // 外部源(mp3api/YouTube)音频代理

	// 轻量健康检查：容器 healthcheck 用它，不再拉一整个 HTML 页面
	handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok"))
	})

	// Legacy favorite API (backward compatible)
	handle("/api/favorite/add", HandleAddToFavorite)
	handle("/api/favorite/remove", HandleRemoveFromFavorite)
	handle("/api/favorite/list", HandleGetFavorites)
	handle("/api/favorite/check", HandleCheckFavorite)

	// User authentication API（登录/注册按 IP 限流，防爆破与 bcrypt CPU 打满）
	loginLimiter := newRateLimiter(10, time.Minute)
	registerLimiter := newRateLimiter(5, 10*time.Minute)
	handle("/api/auth/register", limitByIP(registerLimiter, HandleRegister))
	handle("/api/auth/login", limitByIP(loginLimiter, HandleLogin))
	handle("/api/auth/logout", HandleLogout)
	handle("/api/auth/me", HandleGetCurrentUser)
	// 把 localStorage 里的登录 token 换成 Cookie（运维页面用）
	handle("/api/auth/cookie", HandleIssueCookie)

	// User playlist API (requires authentication)
	handle("/api/user/playlists", AuthMiddleware(HandleGetUserPlaylists))
	handle("/api/user/playlist/songs", AuthMiddleware(HandleGetPlaylistSongs)) // 按歌单名返回精简歌曲列表（ESP32播放用）
	handle("/api/user/playlists/create", AuthMiddleware(HandleCreateUserPlaylist))
	handle("/api/user/playlists/add-song", AuthMiddleware(HandleAddSongToUserPlaylist))
	handle("/api/user/playlists/remove-song", AuthMiddleware(HandleRemoveSongFromUserPlaylist))
	handle("/api/user/playlists/clear", AuthMiddleware(HandleClearUserPlaylistSongs)) // 一键清空歌单内全部歌曲（歌单保留）
	handle("/api/user/playlists/delete", AuthMiddleware(HandleDeleteUserPlaylist))

	// 歌单搬运：导出 m3u/txt/json、导入网易云歌单（导入要打网易云，按 IP 限流）
	handle("/api/user/playlists/export", AuthMiddleware(HandleExportPlaylist))
	handle("/api/user/playlists/import-netease",
		limitByIP(newRateLimiter(10, time.Minute), AuthMiddleware(HandleImportNeteasePlaylist)))

	// 网易云歌单「每天自动增量同步」：登记关系、手动触发、暂停、删除
	handle("/api/user/playlists/sync-jobs", AuthMiddleware(HandleSyncJobs))
	handle("/api/user/playlists/sync-run",
		limitByIP(newRateLimiter(20, time.Minute), AuthMiddleware(HandleSyncRun)))
	handle("/api/user/playlists/sync-toggle", AuthMiddleware(HandleSyncToggle))
	handle("/api/user/playlists/sync-delete", AuthMiddleware(HandleSyncDelete))

	// 游客试听额度：前端先查，额度用完就别发起播放，直接提示登录
	handle("/api/guest/quota", HandleGuestQuota)

	// 播放统计（/status 的图表）
	handle("/api/stats", authOrKey(HandlePlayStats))

	// 正在播放：播放器心跳上报，/status 顶部显示「此刻在放哪首」
	handle("/api/now", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			AuthMiddleware(HandleNowReport)(w, r)
			return
		}
		authOrKey(HandleNowList)(w, r)
	})

	// 音源健康 / 缓存与转码状态（/status 面板用；应急口令也能读只读数据）
	handle("/api/sources", authOrKey(HandleSourcesHealth))

	// 歌词（按需从网易云拉取并落盘缓存，带限流防被当公共接口刷）
	handle("/api/lyric", limitByIP(newRateLimiter(60, time.Minute), HandleLyric))
	// 歌词手动纠错：保存 / 删除覆盖（写操作只认登录态）
	handle("/api/lyric/custom",
		limitByIP(newRateLimiter(30, time.Minute), AuthMiddleware(HandleCustomLyric)))

	// 外部音源（mp3api / YouTube）KEY：/tools 页面填写，即时生效
	// 认登录态或 ADMIN_PAGE_TOKEN 应急口令（/tools?key=... 打开的页面要能用）
	handle("/api/settings/ext", authOrKey(HandleExtSettings))
	handle("/api/settings/ext/test",
		limitByIP(newRateLimiter(20, time.Minute), authOrKey(HandleExtSettingsTest)))

	// 歌词源 LrcAPI 地址：默认公共实例，可切自建实例（站长可改）
	handle("/api/settings/lyric", authOrKey(HandleLyricSettings))
	handle("/api/settings/lyric/test",
		limitByIP(newRateLimiter(20, time.Minute), authOrKey(HandleLyricSettingsTest)))

	// 汽水音乐：状态 / 扫码登录（sidecar 是 compose 里的 gomusic 容器）
	handle("/api/qishui/status", authOrKey(HandleQishuiAdmin))
	handle("/api/qishui/qr", authOrKey(HandleQishuiAdmin))

	// 播放历史 / 最近常听
	handle("/api/history", AuthMiddleware(HandleHistory))
	handle("/api/history/report", AuthMiddleware(HandleHistoryReport))
	handle("/api/history/clear", AuthMiddleware(HandleHistoryClear))

	// 数据备份（运维用，需登录）
	handle("/api/backup", authOrKey(HandleBackup))
	handle("/api/backup/run", AuthMiddleware(HandleBackupRun))

	// ESP32 Device management API
	handle("/api/device/generate-code", AuthMiddleware(GenerateBindingCodeHandler)) // 生成绑定码（需要登录）
	handle("/api/device/bind-direct", AuthMiddleware(DirectBindDeviceHandler))      // Web端直接绑定设备（需要登录）
	handle("/api/device/list", AuthMiddleware(ListUserDevicesHandler))              // 列出用户设备（需要登录）
	handle("/api/device/unbind", AuthMiddleware(UnbindDeviceHandler))               // 解绑设备（需要登录）
	handle("/api/esp32/bind", BindDeviceHandler)                                   // ESP32绑定设备
	handle("/api/esp32/verify", VerifyDeviceHandler)                               // 验证设备Token
	handle("/api/esp32/sync", SyncDeviceHandler)                                   // ESP32同步Token（用MAC地址）

	// 设备遥控：Web 下发指令 → 设备长轮询取走 → 回报执行结果
	handle("/api/device/command", AuthMiddleware(HandleDeviceCommand))
	handle("/api/device/command/status", AuthMiddleware(HandleDeviceCommandStatus))
	handle("/api/esp32/poll", HandleEsp32Poll)
	handle("/api/esp32/ack", HandleEsp32Ack)

	// 运维页面：需要登录（/remote 能下发播放指令，/status 能看到设备与缓存分布）
	// 应急入口：.env 设 ADMIN_PAGE_TOKEN 后可用 ?key=口令 访问
	handle("/status", requireLoginPage(serveThemeFile("status.html")))
	handle("/remote", requireLoginPage(serveThemeFile("device-control.html")))
	handle("/tools", requireLoginPage(serveThemeFile("tools.html")))
	// 歌词页是普通用户功能，保持公开
	handle("/lyrics", serveThemeFile("lyrics.html"))

	// PWA：可安装到手机主屏（manifest / Service Worker / 图标）
	handle("/manifest.webmanifest", HandleManifest())
	handle("/sw.js", HandleServiceWorker())
	handle("/icons/", HandleIcons)

	// 缓存回收：按 TTL + 容量上限在后台清理 files/cache
	startCacheGC()

	// 每天自动备份数据 + 夜间预热歌单
	startMaintenance()

	// 每天自动同步网易云歌单新增曲目
	startSyncScheduler()

	// Device binding page
	handle("/device-bind", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./theme/device-bind.html")
	})
	// 兼容用户常见的拼写错误
	handle("/device-bin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/device-bind", http.StatusMovedPermanently)
	})
	
	fmt.Printf("[Info] %s Started.\n喵波音律-音乐家园QQ交流群:865754861\n", TAG)
	fmt.Printf("[Info] Starting music server at port %s\n", port)

	// Create a channel to listen for signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Create a server instance
	srv := &http.Server{
		Addr:              ":" + port,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,                // Disable the timeout for the response writer
		IdleTimeout:       30 * time.Minute, // Set the maximum duration for idle connections
		ReadHeaderTimeout: 10 * time.Second, // Limit the maximum duration for reading the headers of the request
		MaxHeaderBytes:    1 << 16,          // Limit the maximum request header size to 64KB
	}

	// Start the server
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			fmt.Println(err)
			sigChan <- syscall.SIGINT // Send a signal to shut down the server
		}
	}()

	// Create a channel to listen for standard input
	exitChan := make(chan struct{})

	go func() {
		for {
			var input string
			n, err := fmt.Scanln(&input)
			if err != nil || n == 0 {
				// stdin 关闭或不可读（后台运行），退出监听避免空转
				return
			}
			if input == "exit" {
				exitChan <- struct{}{}
				return
			}
		}
	}()

	// Monitor signals or exit signals from standard inputs
	select {
	case <-sigChan:
		fmt.Printf("[Info] Server is shutting down.\nGoodbye!\n")
	case <-exitChan:
		fmt.Printf("[Info] Server is shutting down.\nGoodbye!\n")
	}

	// Shut down the server
	if err := srv.Shutdown(context.Background()); err != nil {
		fmt.Println(err)
	}
	// 把最后几秒的额度计数与播放记录落盘（正常每 10 秒会自己写一次）
	flushQuota()
	flushHistory()
	flushStats()
	flushGuest()
}
