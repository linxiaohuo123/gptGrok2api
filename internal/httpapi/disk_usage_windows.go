//go:build windows

// [INPUT]: 仅标准库
// [OUTPUT]: diskUsage（Windows 桩实现）
// [POS]: Windows 下返回 0，容量信息由部署侧提供；与 disk_usage_unix.go 成对。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

// Windows builds keep the image statistics available; disk capacity is filled
// by the server-side implementation on Linux deployments.
func diskUsage(string) (total, used, free uint64) {
	return 0, 0, 0
}
