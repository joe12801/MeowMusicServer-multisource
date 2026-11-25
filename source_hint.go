package main

// 「指定音源」提示
//
// 场景：用户对小智说「油管的长城」，希望这首歌只从 mp3api（YouTube 的 yt-dlp 接口）取，
// 不要在国内源里找同名曲，也不要被 GDStudio 塞一个同名不同人的版本。
//
// 两个入口都支持：
//  1. 查询参数 source=mp3api（新版固件会把语音里的「油管」翻译成这个参数）；
//  2. 歌名开头的标记（老固件、网页直接调用都能用）：「油管的长城」→ song=长城。
//
// 只认开头的标记，不认歌名中间出现的词，避免误伤《YouTuber》这类正常歌名。

import "strings"

// 前缀标记 → 规范音源名。命中后会把标记本身和其后的连接词（的/上/里/：/-）一起去掉。
var sourceHintPrefixes = []struct {
	prefix string
	source string
}{
	{"youtube", "mp3api"},
	{"you tube", "mp3api"},
	{"yt music", "mp3api"},
	{"ytmusic", "mp3api"},
	{"油管", "mp3api"},
	{"水管", "mp3api"},
	{"mp3api", "mp3api"},
}

// 标记后面常见的连接词，切掉它们才能拿到真正的歌名。
var sourceHintSeparators = []string{"的", "上", "里", "儿", "：", ":", "-", "—", "，", ",", " ", "　"}

// knownSourceHint 把各种写法归一成链路内部使用的音源名；认不出来返回空。
// 只接受已知值，避免把歌手名（如 Beyond）误当成音源名。
func knownSourceHint(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "youtube", "you tube", "yt", "ytmusic", "油管", "水管", "mp3api":
		return "mp3api"
	}
	return ""
}

// splitSourceHint 从歌名/歌手/显式参数里解析「指定音源」。
// 返回清理后的歌名、歌手，以及要强制使用的音源（空表示不指定）。
func splitSourceHint(song, singer, param string) (string, string, string) {
	song = strings.TrimSpace(song)
	singer = strings.TrimSpace(singer)

	if forced := knownSourceHint(param); forced != "" {
		return song, singer, forced
	}
	// 老固件把「油管」和歌名一起塞进 song_name 时，会落在歌名里；
	// 极少数情况下模型会把它塞进 artist，也一并认掉。
	if forced := knownSourceHint(singer); forced != "" {
		return song, "", forced
	}

	lower := strings.ToLower(song)
	for _, h := range sourceHintPrefixes {
		if !strings.HasPrefix(lower, h.prefix) {
			continue
		}
		rest := strings.TrimSpace(song[len(h.prefix):])
		// 反复切掉连接词：「油管的长城」→「长城」，「youtube - 长城」→「长城」
		for {
			before := rest
			for _, sep := range sourceHintSeparators {
				if strings.HasPrefix(rest, sep) {
					rest = strings.TrimSpace(strings.TrimPrefix(rest, sep))
				}
			}
			if rest == before {
				break
			}
		}
		// 去掉标记后必须是有效歌名，否则当成普通歌名处理（避免「油管」两个字被吃掉）
		if rest == "" {
			return song, singer, ""
		}
		return rest, singer, h.source
	}

	return song, singer, ""
}
