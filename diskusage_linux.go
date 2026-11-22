//go:build linux

package main

import "syscall"

// diskUsage 返回 path 所在文件系统的（可用, 总容量）字节数。
func diskUsage(path string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs
}
