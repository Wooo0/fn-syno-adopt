// Package shares 把群晖共享文件夹迁入飞牛个人空间目录，使其在「我的文件」中显示。
//
// 原理（真机验证 2026-10-30）：飞牛「我的文件」= /volN/<uid>/ 目录树，文件服务做纯
// 目录扫描，没有注册表（测试目录在全部 10 个 PostgreSQL 库零命中）。把数据根顶层的
// 共享文件夹（btrfs 子卷）mv 进该目录即显示为文件夹——子卷目录同样被识别（probe-sub
// 实验全过：显示为文件夹、内部文件可打开）。此前软链方案失败（链接被识别为文件类型）。
//
// mv 在同一文件系统内是 rename：瞬间完成、零拷贝、与数据量无关；挪回去即可还原。
// 但飞牛的个人空间目录 /volN/<uid> 本身通常是**独立子卷**，此时 rename 必返回
// EXDEV，只能退化成 reflink 克隆 + 删源（见 cloneAcrossSubvol）。
// 代价：回群晖时路径从 @syno/<name> 变为 @syno/<uid>/<name>，DSM 共享文件夹需重指
// （数据无损，用户已明确接受）。
package shares

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/owner"
)

// Item 数据根顶层的一个目录条目。
type Item struct {
	Name     string `json:"name"`
	IsSubvol bool   `json:"isSubvol"` // btrfs 子卷（inode 恒为 256）
	System   bool   `json:"system"`   // 系统目录：一律拒绝迁移
}

// View 一个已接管卷的共享文件夹扫描结果。
type View struct {
	MountPoint string   `json:"mountPoint"`
	UID        string   `json:"uid"`       // 默认个人空间目录名（第一个数字目录，如 1000）
	UIDDirs    []string `json:"uidDirs"`   // 顶层数字目录全集（多用户场景）
	UIDExists  bool     `json:"uidExists"` // 个人空间目录是否已存在
	Items      []Item   `json:"items"`     // 可迁移候选
	System     []Item   `json:"system"`    // 系统目录（只展示，不给迁）
	Migrated   []string `json:"migrated"`  // 已迁入个人空间的目录（只读展示）
}

// mpRE 挂载点白名单：只接受 /volN 形式，杜绝路径拼接类误用。
var mpRE = regexp.MustCompile(`^/vol\d+$`)

// sysNames 群晖系统目录名（normalize 后比较）。怪名家族 @\eadir\ @\database\ 等
// 剥掉斜杠后正好命中；@ / # 前缀另有独立判断。
var sysNames = map[string]bool{
	"eadir": true, "database": true, "s2s": true, "tmp": true,
	"recycle": true, "docker": true,
}

// normalize 剥掉名字里的斜杠并转小写——怪名家族的统一比较方式
// （与 adopt.IsSynoSubvolName 同思路：显示层看着是 @\eadir\，实际字节含反斜杠）。
func normalize(name string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name))
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isSubvol btrfs 子卷的 inode 恒为 256，普通目录不是（实现按平台拆分）。

// Scan 扫描一个已接管卷的数据根顶层，产出可迁移候选与现状。
func Scan(mountPoint string) (*View, error) {
	if !mpRE.MatchString(mountPoint) {
		return nil, fmt.Errorf("挂载点格式非法（应为 /volN）: %q", mountPoint)
	}
	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", mountPoint, err)
	}
	v := &View{
		MountPoint: mountPoint,
		UIDDirs:    []string{}, Items: []Item{}, System: []Item{}, Migrated: []string{},
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !e.IsDir() {
			continue
		}
		if isDigits(name) {
			v.UIDDirs = append(v.UIDDirs, name)
			continue
		}
		sub := isSubvol(filepath.Join(mountPoint, name))
		sys := strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") || sysNames[normalize(name)]
		if sys {
			v.System = append(v.System, Item{Name: name, IsSubvol: sub, System: true})
		} else {
			v.Items = append(v.Items, Item{Name: name, IsSubvol: sub})
		}
	}
	sort.Slice(v.UIDDirs, func(i, j int) bool { return v.UIDDirs[i] < v.UIDDirs[j] })
	if len(v.UIDDirs) > 0 {
		v.UID = v.UIDDirs[0]
		v.UIDExists = true
		// 已迁入个人空间的目录：只读展示。多用户目录只看第一个——当前场景
		// 单用户（1000），多用户需求出现时再扩展。
		if inner, err := os.ReadDir(filepath.Join(mountPoint, v.UID)); err == nil {
			for _, e := range inner {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					v.Migrated = append(v.Migrated, e.Name())
				}
			}
		}
	}
	return v, nil
}

// MoveResult 单个目录的迁移结果。
type MoveResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Note 成功时的补充说明。跨子卷迁移不是 rename 而是"克隆+删源"，
	// 与同子卷的瞬间 rename 语义不同，必须让用户知道发生了什么。
	Note string `json:"note,omitempty"`
	Err  string `json:"err,omitempty"`
}

// Move 把选中的目录 rename 进个人空间目录（/volN/<uid>/）。
// 逐项独立执行、结果逐项返回；系统目录/个人空间目录/非法名一律拒绝；
// 目标已存在一律拒绝（绝不覆盖）。个人空间目录不存在时自动创建并对齐属主。
func Move(mountPoint, uid string, names []string) ([]MoveResult, error) {
	if !mpRE.MatchString(mountPoint) {
		return nil, fmt.Errorf("挂载点格式非法（应为 /volN）: %q", mountPoint)
	}
	if !isDigits(uid) {
		return nil, fmt.Errorf("个人空间目录名非法（应为纯数字 uid）: %q", uid)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("未选择任何目录")
	}
	uidDir := filepath.Join(mountPoint, uid)
	if _, err := os.Stat(uidDir); os.IsNotExist(err) {
		if err := os.MkdirAll(uidDir, 0o755); err != nil {
			return nil, fmt.Errorf("创建个人空间目录 %s 失败: %w", uidDir, err)
		}
		// 对齐飞牛习惯（probe-test 属主为普通用户）：属主设为本地用户，
		// 避免文件服务对 root 属主目录出现识别异常。失败不致命，继续。
		if u := owner.Suggest(); u != nil {
			_ = os.Chown(uidDir, u.UID, u.GID)
		}
	}
	out := make([]MoveResult, 0, len(names))
	for _, name := range names {
		r := MoveResult{Name: name}
		switch {
		case name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\'):
			r.Err = "名字非法"
		case strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") || sysNames[normalize(name)]:
			r.Err = "系统目录不可迁移"
		case isDigits(name):
			r.Err = "个人空间目录不可迁移"
		}
		if r.Err == "" {
			src := filepath.Join(mountPoint, name)
			dst := filepath.Join(uidDir, name)
			if _, err := os.Lstat(dst); err == nil {
				r.Err = "目标已存在，拒绝覆盖: " + dst
			} else if err := os.Rename(src, dst); err != nil {
				// rename(2) 跨子卷一律 EXDEV：源在 @syno 里，目标 /volN/<uid> 往往是
				// 另一个子卷（飞牛的个人空间本身就是一个子卷）。这不是盘坏了，是内核
				// 不允许把目录从一个子卷挪进另一个子卷。真机 2026-10-31 就报成
				// "invalid cross-device link"，用户完全看不懂。
				if !errors.Is(err, syscall.EXDEV) {
					r.Err = err.Error()
				} else if cerr := cloneAcrossSubvol(src, dst); cerr != nil {
					r.Err = "跨子卷迁移失败: " + cerr.Error()
				} else {
					r.OK = true
					r.Note = "源与目标不在同一子卷，内核不允许 rename；已用 reflink 克隆（同一文件系统内不复制实际数据）后删除原目录"
				}
			} else {
				r.OK = true
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// cloneAcrossSubvol 处理「源与目标分属不同 btrfs 子卷」的迁移。
//
// 背景（真机 2026-10-31）：/vol5 上 `1000` 是一个独立子卷（飞牛个人空间），
// 把 `Download` 从 `@syno` 挪进去时 rename(2) 直接返回 EXDEV，
// 界面上只显示 "invalid cross-device link"。用户看到这句话无法判断盘坏没坏。
//
// rename 跨子卷之所以必失败：两个目录项属于不同的 btrfs 子树（root 不同），
// 内核无法只改父目录的 dentry 就完成"移动"，这是设计上的限制，与权限、空间、
// 挂载选项都无关。
//
// 做法分三步，保证任何一步失败都不会留下一个"半截目标"挡住重试：
//  1. 复制到同目录下的临时名（cp -a --reflink=auto）
//     —— 同一文件系统内 reflink 只写元数据，几十 GB 也是秒级；
//     文件系统不支持克隆时自动退化成真拷贝（慢但正确）
//  2. 在**同一子卷内**把临时名改名成目标名（这是普通 rename，必成功）
//  3. 删除源目录；删不掉也如实报告，绝不假装成功
func cloneAcrossSubvol(src, dst string) error {
	tmp := dst + ".fn-syno-migrating"
	// 上次失败可能留下残骸。只在临时名上动手，绝不碰用户数据。
	if _, err := os.Lstat(tmp); err == nil {
		if err := os.RemoveAll(tmp); err != nil {
			return fmt.Errorf("清理上次残留 %s 失败: %w", tmp, err)
		}
	}

	// 超时给足：真拷贝几百 GB 可能要数小时，而 reflink 通常几秒返回。
	// 超时后不会继续删源，用户重试即可。
	if _, err := commander.Run(12*time.Hour, "cp", "-a", "--reflink=auto", "--", src, tmp); err != nil {
		_ = os.RemoveAll(tmp) // 半截临时目录留着没意义，且会挡住下次重试
		return fmt.Errorf("复制 %s → %s 失败（原目录未删除）: %w", src, tmp, err)
	}

	if err := os.Rename(tmp, dst); err != nil {
		// 数据已经完整复制在 tmp 里，只是改名失败——把位置告诉用户
		return fmt.Errorf("已完整复制到 %s，但改名为 %s 失败（原目录未删除，可手工 mv）: %w",
			tmp, dst, err)
	}

	// 删源必须分两种情况：源本身是 btrfs 子卷时，rmdir 会返回 EPERM，
	// 而 os.RemoveAll 会**先把子卷里的内容删光**再失败——那不是报错，
	// 那是静默丢数据。所以子卷一律走 btrfs subvolume delete。
	if isSubvol(src) {
		if _, err := commander.Run(2*time.Hour, "btrfs", "subvolume", "delete", src); err != nil {
			return fmt.Errorf("已复制并改名为 %s，但删除原子卷 %s 失败。"+
				"数据在目标处完整无损，请手工执行: btrfs subvolume delete %s", dst, src, src)
		}
		return nil
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("已复制并改名为 %s，但删除原目录 %s 失败，请手工删除: %w", dst, src, err)
	}
	return nil
}
