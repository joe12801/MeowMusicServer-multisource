//go:build !linux

package main

// diskUsage 非 Linux 平台（本地 Windows 调试）不做统计，返回 0 让前端隐藏该行。
func diskUsage(path string) (free, total int64) {
	return 0, 0
}
