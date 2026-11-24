package main

// 转码进程的调度策略。
//
// 现场问题（2026-11-24）：音箱播「已经缓存好」的歌也会一卡一卡。
// 抓 ps 看到容器里 ffmpeg 常驻 90%+ CPU（预热/回填一直在转码），
// 宿主机上还有 NVR 的三路 rtsp→aac 转码，load 长期 2.6+。
// 这台 N1 只有 4 个 A53 核，Go 的 HTTP 发送协程被调度延迟几百毫秒，
// ESP32 那点接收缓冲瞬间抽干 —— 表现就是"卡一下、又顺一下"。
//
// 两条措施：
//   1) 转码输出是 24kHz/32kbps 单声道，单线程足够，不再用 -threads 0 吃满核；
//   2) ffmpeg 以 nice 19 跑，播放时的网络发送永远抢得过转码。

import (
	"context"
	"os/exec"
	"sync"
	"sync/atomic"
)

// audioStreams 统计正在进行的音频发送（整包发送 + 实时转码流）。
//
// ESP32 消费极慢（32kbps），一个连接会持续整首歌，所以这个计数
// 能相当准确地回答「现在有没有人在听」。后台预热靠它让路。
var audioStreams atomic.Int64

func audioBusy() bool { return audioStreams.Load() > 0 }

// ffmpegBinary 返回可执行文件路径：优先 nice 包装，没有就裸跑。
var ffmpegBinary = sync.OnceValue(func() string {
	if _, err := exec.LookPath("nice"); err == nil {
		return "nice"
	}
	return "ffmpeg"
})

// ffmpegCmd 构造一条低优先级的 ffmpeg 命令。
// ctx 为 nil 时不带超时取消（用于请求生命周期之内的转码）。
func ffmpegCmd(ctx context.Context, args ...string) *exec.Cmd {
	if ffmpegBinary() == "nice" {
		args = append([]string{"-n", "19", "ffmpeg"}, args...)
	}
	if ctx != nil {
		return exec.CommandContext(ctx, ffmpegBinary(), args...)
	}
	return exec.Command(ffmpegBinary(), args...)
}
