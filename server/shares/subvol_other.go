//go:build !linux

package shares

// isSubvol 非 Linux 平台（开发机单元测试）无从读 inode，一律按普通目录处理。
func isSubvol(string) bool { return false }
