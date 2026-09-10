//go:build !windows

// [INPUT]: 仅标准库（syscall）
// [OUTPUT]: diskUsage（Unix 实现）
// [POS]: 磁盘容量查询的平台分支，与 disk_usage_windows.go 成对。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"os"
	"syscall"
)

func diskUsage(path string) (total, used, free uint64) {
	if _, err := os.Stat(path); err != nil {
		return 0, 0, 0
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0
	}
	blockSize := uint64(stat.Bsize)
	total = stat.Blocks * blockSize
	free = stat.Bavail * blockSize
	if total >= free {
		used = total - free
	}
	return total, used, free
}
