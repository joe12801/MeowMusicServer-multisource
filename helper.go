package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Source is an alias for MusicItem (used in sources.json)
type Source = MusicItem

// Download file from URL
func downloadFile(filepath string, url string) error {
	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

// Get IP address from request
func IPhandler(r *http.Request) (string, error) {
	ip := r.Header.Get("X-Real-IP")
	if ip == "" {
		ip = r.Header.Get("X-Forwarded-For")
	}
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return ip, nil
}

// Read sources from sources.json
func readSources() []Source {
	file, err := os.Open("sources.json")
	if err != nil {
		return []Source{}
	}
	defer file.Close()

	var sources []Source
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&sources)
	if err != nil {
		return []Source{}
	}
	return sources
}

// Read music from cache
func readFromCache(path string) (MusicItem, bool) {
	// Logic to read music item from a cached folder path
	// This assumes path is like "files/cache/music/Artist-Song"
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return MusicItem{}, false
	}

	dirName := filepath.Base(path)
	parts := strings.SplitN(dirName, "-", 2)
	var artist, title string
	if len(parts) == 2 {
		artist = parts[0]
		title = parts[1]
	} else {
		title = dirName
	}

	return getLocalMusicItem(title, artist), true
}

// srcEnabled 判断某个音源是否被启用（默认全开，可用 MUSIC_SOURCES 收窄便于排障）
func srcEnabled(name string) bool {
	list := strings.TrimSpace(os.Getenv("MUSIC_SOURCES"))
	if list == "" {
		return true
	}
	for _, s := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(s), name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 多源共用的「版本噪声」判定
//
// 起因：酷狗/网易云/QQ 的免费曲池里，同一首歌被大量「二次加工版」占满——
// 点《成都》给「(2026没有信号巡回演唱会北京站)」、点《大鱼》给「(3D环绕版)」、
// 点《山海》给「(DJ LayE版)」。这些标题里歌名和歌手都完全对得上，只靠
// 「歌名归一化 + 歌手匹配」打分会全部判成原唱，设备就放成了演唱会现场。
//
// 所以判定分两层：
//  1. 关键词黑名单（现场/伴奏/改编/DJ…），命中即 fatal；
//  2. 括号后缀白名单。标题括号里如果写了别的东西，基本就是在告诉你
//     「这不是原版」；只有「原版/正式版/单曲/Album Version」这类
//     纯标注性后缀才放行（如《海底 (Single Version)》确实是原曲）。
//
// 注意括号必须用**原始标题**取，不能用归一化后的——gdNorm 会把括号
// 连同内容整段删掉，等于把最重要的版本信息丢了。
// ---------------------------------------------------------------------------

var srcNoiseWords = []string{
	"伴奏", "伴唱", "消音", "纯音乐", "ktv", "karaoke", "instrumental",
	"dj", "铃声", "片段", "试听", "升调", "降调", "加速", "慢速", "变速",
	"女声", "男声", "童声", "童音", "女版", "男版", "钢琴", "吉他", "古筝",
	"二胡", "小提琴", "大提琴", "萨克斯", "口琴", "八音盒", "陶笛", "葫芦丝",
	"架子鼓", "尤克里里", "长笛",
	"合辑", "串烧", "remix", "混音", "改编", "复刻", "高仿",
	"车载", "慢摇", "慢四", "慢三", "快四", "舞曲", "广场舞", "交谊舞",
	"抖音", "快手", "热播", "热歌", "网红", "鬼畜", "变声", "喊麦", "蹦迪",
	"cctv", "音乐频道", "电台", "春晚", "综艺", "音综", "节目", "纯享",
	"饭制", "剪辑", "超燃", "燃版", "高燃",
	"翻自", "翻唱", "cover", "live", "现场", "演唱会", "音乐会", "生日会",
	"3d", "8d", "环绕", "阿卡贝拉", "acapella", "清唱", "demo", "伴奏带",
	"0.8x", "0.9x", "1.1x", "1.2x", "降b", "升f",
}

var srcInstrumentalWords = []string{
	"伴奏", "伴唱", "消音", "纯音乐", "ktv", "karaoke", "instrumental",
}

// srcVersionWhitelist 括号里出现这些词，说明只是版本标注而不是二次加工
var srcVersionWhitelist = []string{
	"原版", "正式版", "单曲", "single", "studio", "album version",
	"专辑版", "完整版", "高音质", "无损", "original version", "explicit",
	"main version", "album ver",
}

// srcBracketSuffix 取标题里第一段括号内容（半角/全角/方头括号都认）
func srcBracketSuffix(name string) string {
	pairs := [][2]string{{"(", ")"}, {"（", "）"}, {"[", "]"}, {"【", "】"}}
	for _, p := range pairs {
		i := strings.Index(name, p[0])
		if i < 0 {
			continue
		}
		j := strings.Index(name[i:], p[1])
		if j < 0 {
			return strings.TrimSpace(name[i+len(p[0]):])
		}
		return strings.TrimSpace(name[i+len(p[0]) : i+j])
	}
	return ""
}

// srcTitleScore 歌名比对，返回 (分数, 是否 fatal)。
//
// 「包含匹配」不能直接给高分：QQ/酷狗上大量结果是**视频标题**而不是歌曲名，
// 典型如搜《麒麟》拿到「《麒麟》我自有一口气 一剑开天门」（102 秒切片）。
// 这类标题把歌名当引子，后面挂一长串描述，归一化后仍然「包含」歌名，
// 只按包含关系打分会稳稳命中。所以再加一道长度比例闸门：
// 标题比歌名长出一大截（超过 1.6 倍）就不认。
func srcTitleScore(gotName, wantName string) (int, bool) {
	switch {
	case gotName == wantName:
		return 100, false
	case strings.Contains(gotName, wantName) || strings.Contains(wantName, gotName):
		gr := len([]rune(gotName))
		wr := len([]rune(wantName))
		if wr > 0 && gr > wr*8/5 {
			return -80, true
		}
		return 50, false
	default:
		return -45, false
	}
}

// srcNoiseOf 返回 (扣分, 是否 fatal, 是否无人声)
func srcNoiseOf(name, album, song string) (int, bool, bool) {
	lowerName := strings.ToLower(name)
	lowerAlbum := strings.ToLower(album)
	lowerSong := strings.ToLower(song)

	for _, w := range srcNoiseWords {
		if strings.Contains(lowerSong, w) {
			continue // 用户自己就在搜伴奏/DJ 版
		}
		if strings.Contains(lowerName, w) || strings.Contains(lowerAlbum, w) {
			noVocal := false
			for _, iw := range srcInstrumentalWords {
				if w == iw {
					noVocal = true
					break
				}
			}
			return -100, true, noVocal
		}
	}

	// 括号后缀不在白名单，当作二次加工版处理。
	// 用户自己搜的歌名里就带这个后缀时放行（例如直接搜「起风了 (旧版)」）。
	if suffix := strings.ToLower(srcBracketSuffix(name)); suffix != "" &&
		!strings.Contains(lowerSong, suffix) {
		okSuffix := false
		for _, w := range srcVersionWhitelist {
			if strings.Contains(suffix, w) {
				okSuffix = true
				break
			}
		}
		if !okSuffix {
			return -90, true, false
		}
	}
	return 0, false, false
}

// Request and cache music from API
//
// 分两段：前段全是「严格模式」，只收真原唱；后段才是宽容兜底。
// 这样《理想三旬》这种酷我只收了 Live 版/翻唱版的情况，会先把机会
// 让给 QQ / 网易云（它们有 210 秒的原版），而不是直接放半首 Live。
//
//  1. 酷我（严格）—— 唯一能覆盖付费大牌（周杰伦/陈奕迅）的通道，返回完整 320k，
//     正常 0.5s 命中，绝大多数点歌在这一步结束。
//  2. 网易云直连 —— 不依赖本地服务，覆盖「有版权且免 VIP」的曲目。
//  3. 小枫 xfabe —— 同曲库，但由第三方后端代取链，能补网易云的 VIP 曲；
//     接口有限流，撞到 405 会自动静默 15 分钟，不影响后面几个源。
//  4. QQ 音乐 —— 只对免费曲有效，但对独立/民谣/网络歌曲覆盖极好（实测冷门 6/10）。
//  5. 酷狗     —— 曲库最厚，但热门曲绝大多数要付费，实测冷门仅 1/10，只作补充。
//  6. 网易云本地 API —— 若同机跑了 ncm 服务则用（带 UNM 解灰），没跑则瞬间返回空。
//  7. 酷我（宽容）—— 上面全空时，用 Live/翻唱版顶一下，能听比没歌强。
//  8. GD Studio —— 公益聚合源，境外环境下是唯一能用的（酷我/酷狗/QQ 都限大陆）。
//  9. mp3.994938.xyz —— 自建 yt-dlp 接口，关键词直搜取第一条，国内外都可用；
//     冷启动 13s+，上游缓存后秒回，作 GDStudio 之后的兜底。
// 10. yuafeng   —— 最后兜底，能出什么算什么。
//
// 临时调整：设 MUSIC_MP3API_FIRST=1 可把 mp3.994938.xyz 提到整条链路最前
// （其它源仍在后面兜底），去掉环境变量重启即恢复，不必回滚二进制。
func requestAndCacheMusic(song, singer string) MusicItem {
	first := mp3ApiFirst()
	fmt.Printf("[Source] %q - %q | kuwo=%v kugou=%v netease=%v xfabe=%v qq=%v gdstudio=%v mp3api=%v first=%v\n",
		song, singer, srcEnabled("kuwo"), srcEnabled("kugou"),
		srcEnabled("netease"), srcEnabled("xfabe"), srcEnabled("qq"),
		srcEnabled("gdstudio"), srcEnabled("mp3api"), first)

	// 临时前置：先试自建 yt-dlp 源，命中就直接返回；没命中继续原有链路。
	// 后段不再重复探一次，避免同一请求里双倍冷启动等待。
	mp3ApiTried := false
	if first && srcEnabled("mp3api") {
		mp3ApiTried = true
		fmt.Printf("[Source] mp3api 临时前置优先\n")
		if item := MP3ApiSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}

	// ---- 前段：只要真原唱 ----
	if srcEnabled("kuwo") {
		if item := KuwoSearchStrict(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	if srcEnabled("netease") {
		if item := NeteaseSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	// 小枫 xfabe：与网易云同曲库，但能代取 VIP 曲的直链（限流时会自己闭嘴）
	if srcEnabled("xfabe") {
		if item := XfabeSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	if srcEnabled("qq") {
		if item := QQMusicSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	if srcEnabled("kugou") {
		if item := KugouSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	// 本地网易云 API（带 UNM 解灰，能跨源补齐无版权歌曲）
	if srcEnabled("netease") {
		if item := NCMSearch(song, singer); item.Title != "" {
			return item
		}
	}

	// ---- 后段：宽容兜底 ----
	if srcEnabled("kuwo") {
		if item := KuwoSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	// 前面都不通时退回 GD Studio 公益音源（境外主要靠它）
	if srcEnabled("gdstudio") {
		if item := GDStudioSearch(song, singer); item.Title != "" {
			return item
		}
	}
	// mp3.994938.xyz（yt-dlp 兜底）：YouTube 曲库关键词直搜取第一条，
	// 国内/国外服务器都能直连（境外实测比境内还快）。首次冷启动 13s+，
	// 上游缓存后秒回，故只放后段兜底。KEY 走 MUSIC_MP3API_KEY，未配置自动跳过。
	if srcEnabled("mp3api") && !mp3ApiTried {
		if item := MP3ApiSearch(song, singer); item.Title != "" && item.AudioURL != "" {
			return item
		}
	}
	// 最后兜底：yuafeng 免费接口
	if srcEnabled("yuafeng") {
		for _, source := range []string{"kuwo", "netease", "migu", "baidu"} {
			if item := YuafengAPIResponseHandler(source, song, singer); item.Title != "" {
				return item
			}
		}
	}
	return MusicItem{}
}

// 直接从远程URL流式转码（边下载边转码，超快！）
func streamConvertAudio(inputURL, outputFile string) error {
	fmt.Printf("[Info] Stream converting from URL (fast mode)\n")

	// 先写入临时文件，完成后再重命名（避免读取到不完整的文件）
	tempFile := outputFile + ".tmp"

	// ffmpeg 直接读取远程 URL 并转码
	// -t 600: 只下载前10分钟，减少80%下载量！
	// 移除 reconnect 参数，避免兼容性问题
	// 添加 -bufsize 以提高稳定性
	cmd := exec.Command("ffmpeg", "-y",
		"-t", "600",
		"-i", inputURL,
		"-threads", "0",
		"-ac", "1", "-ar", "24000", "-b:a", "32k", "-q:a", "9",
		"-bufsize", "64k",
		tempFile)

	err := cmd.Run()
	if err != nil {
		fmt.Printf("[Error] Stream convert failed: %v\n", err)
		os.Remove(tempFile) // 清理临时文件
		return err
	}

	// 检查生成的文件大小
	fileInfo, err := os.Stat(tempFile)
	if err != nil || fileInfo.Size() < 1024 {
		fmt.Printf("[Error] Stream converted file is too small or empty\n")
		os.Remove(tempFile)
		return fmt.Errorf("converted file is too small")
	}

	// 转码完成后重命名为最终文件
	err = os.Rename(tempFile, outputFile)
	if err != nil {
		fmt.Printf("[Error] Failed to rename temp file: %v\n", err)
		return err
	}

	fmt.Printf("[Success] Stream convert completed: %s\n", outputFile)
	return nil
}

// 实时流式转码到 HTTP Writer（边下载边播放！）
func streamConvertToWriter(inputURL string, w http.ResponseWriter) error {
	fmt.Printf("[Info] Live streaming from URL: %s\n", mp3ApiMaskKey(inputURL))

	// ffmpeg 边下载边转码，输出到 stdout
	cmd := exec.Command("ffmpeg",
		"-i", inputURL,
		"-threads", "0",
		"-ac", "1", "-ar", "24000", "-b:a", "32k", "-q:a", "9",
		"-f", "mp3",
		"-map_metadata", "-1",
		"pipe:1") // 输出到 stdout

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	// ESP32 的 esp_http_client 解析 chunked 长流会在中途报
	// "Parse chunk size failed" 并断流（实测每首只收到约 100KB 就停）。
	// 这里 Hijack 底层连接，手写不带 Transfer-Encoding 的 HTTP/1.1 响应头，
	// 用 Connection: close 让客户端按 EOF 判定结束，彻底绕开 chunk 解析。
	if hj, ok := w.(http.Hijacker); ok {
		conn, bufrw, herr := hj.Hijack()
		if herr == nil {
			defer conn.Close()
			_, herr = bufrw.WriteString("HTTP/1.1 200 OK\r\n" +
				"Content-Type: audio/mpeg\r\n" +
				"Accept-Ranges: bytes\r\n" +
				"Connection: close\r\n\r\n")
			if herr == nil {
				herr = bufrw.Flush()
			}
			if herr != nil {
				cmd.Process.Kill()
				cmd.Wait()
				return fmt.Errorf("hijack write header: %v", herr)
			}
			fmt.Printf("[Stream Live] Raw response (no chunked), streaming to EOF\n")
			buf := make([]byte, 16384)
			total := 0
			for {
				n, rerr := stdout.Read(buf)
				if n > 0 {
					if _, werr := conn.Write(buf[:n]); werr != nil {
						break // 客户端断开（切歌/停止）
					}
					total += n
				}
				if rerr != nil {
					break
				}
			}
			cmd.Wait()
			fmt.Printf("[Success] Live streaming completed (raw, %d bytes)\n", total)
			return nil
		}
		fmt.Printf("[Stream Live] Hijack failed: %v, fallback to chunked\n", herr)
	}

	// 兜底：无法 Hijack 时退回普通写法（带 chunked）
	w.Header().Set("Content-Type", "audio/mpeg")
	buf := make([]byte, 8192)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	cmd.Wait()
	fmt.Printf("[Success] Live streaming completed (chunked fallback)\n")
	return nil
}

// Helper function for identifying file formats
func getMusicFileExtension(url string) (string, error) {
	resp, err := http.Head(url)
	if err != nil {
		return "", err
	}
	// Get file format from Content-Type header
	contentType := resp.Header.Get("Content-Type")
	ext, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", err
	}
	// Identify file extension based on file format
	switch ext {
	case "audio/mpeg":
		return ".mp3", nil
	case "audio/flac":
		return ".flac", nil
	case "audio/x-flac":
		return ".flac", nil
	case "audio/wav":
		return ".wav", nil
	case "audio/aac":
		return ".aac", nil
	case "audio/ogg":
		return ".ogg", nil
	case "application/octet-stream":
		// Try to guess file format from URL or other information
		if strings.Contains(url, ".mp3") {
			return ".mp3", nil
		} else if strings.Contains(url, ".flac") {
			return ".flac", nil
		} else if strings.Contains(url, ".wav") {
			return ".wav", nil
		} else if strings.Contains(url, ".aac") {
			return ".aac", nil
		} else if strings.Contains(url, ".ogg") {
			return ".ogg", nil
		} else {
			return "", fmt.Errorf("unknown file format from Content-Type and URL: %s", contentType)
		}
	default:
		return "", fmt.Errorf("unknown file format: %s", ext)
	}
}

// Helper function for identifying file formats
func GetDuration(filePath string) int {
	fmt.Printf("[Info] Get duration of obtaining music file %s\n", filePath)
	// Use ffprobe to get audio duration
	output, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", filePath).Output()
	if err != nil {
		fmt.Println("[Error] Error getting audio duration:", err)
		return 0
	}

	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		fmt.Println("[Error] Error converting duration to float:", err)
		return 0
	}

	return int(duration)
}

// Helper function to compress and segment audio file
func compressAndSegmentAudio(inputFile, outputDir string) error {
	fmt.Printf("[Info] Compress and segment audio file %s\n", inputFile)
	// Compress music files
	outputFile := filepath.Join(outputDir, "music.mp3")
	cmd := exec.Command("ffmpeg", "-i", inputFile, "-ac", "1", "-ab", "32k", "-ar", "24000", outputFile)
	err := cmd.Run()
	if err != nil {
		return err
	}

	return nil
}

// Helper function to obtain music data from local folder
func getLocalMusicItem(song, singer string) MusicItem {
	musicDir := "./files/music"
	fmt.Println("[Info] Reading local folder music.")
	files, err := os.ReadDir(musicDir)
	if err != nil {
		fmt.Println("[Error] Failed to read local music directory:", err)
		return MusicItem{}
	}

	for _, file := range files {
		if file.IsDir() {
			if singer == "" {
				if strings.Contains(file.Name(), song) {
					dirPath := filepath.Join(musicDir, file.Name())
					// Extract artist and title from the directory name
					parts := strings.SplitN(file.Name(), "-", 2)
					var artist, title string
					if len(parts) == 2 {
						artist = parts[0]
						title = parts[1]
					} else {
						title = file.Name()
					}
					basePath := "/cache/music/" + url.QueryEscape(file.Name())
					return MusicItem{
						Title:        title,
						Artist:       artist,
						CoverURL:     basePath + "/cover.jpg",
						LyricURL:     basePath + "/lyric.lrc",
						AudioFullURL: basePath + "/music.mp3",
						AudioURL:     basePath + "/music.mp3",
						M3U8URL:      basePath + "/music.m3u8",
						Duration:     GetDuration(filepath.Join(dirPath, "music.mp3")),
					}
				}
			} else {
				if strings.Contains(file.Name(), song) && strings.Contains(file.Name(), singer) {
					dirPath := filepath.Join(musicDir, file.Name())
					// Extract artist and title from the directory name
					parts := strings.SplitN(file.Name(), "-", 2)
					var artist, title string
					if len(parts) == 2 {
						artist = parts[0]
						title = parts[1]
					} else {
						title = file.Name()
					}
					basePath := "/cache/music/" + url.QueryEscape(file.Name())
					return MusicItem{
						Title:        title,
						Artist:       artist,
						CoverURL:     basePath + "/cover.jpg",
						LyricURL:     basePath + "/lyric.lrc",
						AudioFullURL: basePath + "/music.mp3",
						AudioURL:     basePath + "/music.mp3",
						M3U8URL:      basePath + "/music.m3u8",
						Duration:     GetDuration(filepath.Join(dirPath, "music.mp3")),
					}
				}
			}
		}
	}
	return MusicItem{}
}
