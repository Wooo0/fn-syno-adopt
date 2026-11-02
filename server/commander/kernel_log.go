package commander

import (
	"strings"
	"time"
)

// RecentBtrfsKernelLog 返回最近 n 条与 btrfs 有关的内核日志。
//
// 为什么需要它：mount 失败时 mount(8) 自己只会吐一句
// "wrong fs type, bad option, bad superblock on /dev/mapper/xxx, ... "，
// 真正的原因（比如 "BTRFS error: invalid root flags, have 0x... expect mask 0x..."）
// 只在内核环形缓冲里。不把这句捞出来给用户看，任何"挂载失败"都只能靠猜——
// 而"私有 root flag"和"真坏块"的处理办法完全相反。
//
// 取日志的努力顺序：
//  1. dmesg（最快。kernel.dmesg_restrict=1 时非 root 会 EPERM，本服务是 root，通常可用）
//  2. journalctl -k（部分系统关掉 dmesg 但保留 journal）
//  3. 都拿不到就返回 nil —— 绝不因为"日志取不到"影响主流程
func RecentBtrfsKernelLog(n int) []string {
	if n <= 0 {
		n = 6
	}
	lines := kernelLogLines()
	if len(lines) == 0 {
		return nil
	}
	hit := make([]string, 0, n)
	for _, l := range lines {
		low := strings.ToLower(l)
		if strings.Contains(low, "btrfs") {
			hit = append(hit, clipKernelLine(l, 300))
		}
	}
	if len(hit) == 0 {
		return nil
	}
	if len(hit) > n {
		hit = hit[len(hit)-n:]
	}
	return hit
}

// kernelLogLines 拿到内核日志的原始行（dmesg 优先，journalctl 兜底）。
func kernelLogLines() []string {
	if res, err := Run(10*time.Second, "dmesg"); err == nil && res.Stdout != "" {
		if ls := splitLines(res.Stdout); len(ls) > 0 {
			return ls
		}
	}
	// journalctl -k：只取内核消息，限制行数避免整本日志读进内存
	if res, err := Run(15*time.Second, "journalctl", "-k", "-n", "400", "--no-pager", "-o", "cat"); err == nil {
		if ls := splitLines(res.Stdout); len(ls) > 0 {
			return ls
		}
	}
	return nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// clipKernelLine 截断超长行（内核日志里偶尔会带整段 btrfs 元数据 dump）。
func clipKernelLine(s string, max int) string {
	if max <= 0 {
		max = 300
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
