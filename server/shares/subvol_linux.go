//go:build linux

package shares

import (
	"os"
	"syscall"
)

// isSubvol btrfs 子卷的 inode 恒为 256（mksubvolume 固定分配），普通目录不是。
func isSubvol(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return sys.Ino == 256
	}
	return false
}
