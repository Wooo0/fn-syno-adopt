package adopt

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
)

// 群晖在 btrfs 上留了两类「主核不认识」的私有标志，报错位置、拒读阶段、
// 处置办法全都不同。把它们混成一句"私有 root flag"会给出错误建议——
// 真机 2026-10-31 就是这样误导过一次：
//
//	① root item 的 root flags（bit 34 = 0x400000000）
//	   open_ctree 阶段就拒，报 "invalid root flags, have 0x400000000 expect mask ..."
//	   → 整棵文件系统树都进不去，连子卷列表都列不出来
//
//	② 叶子里的 inode item flags（bit 30 = 0x40000000）
//	   tree-checker 边读边校验，报 "corrupt leaf: root=257 block=... slot=1 ino=8512,
//	   unknown incompat flags detected: 0x40000000"
//	   → 文件系统根能挂、子卷列表能列，唯独目标子卷（树 257）读不了
//
// sdb(vg1001/lv) 与 sdg(vg1002/lv) 中的是 ②。两者都还能救，但办法不同。
var (
	// ① 的原文：invalid root flags, have 0x400000000 expect mask 0x1000000000001
	rootFlagRE = regexp.MustCompile(`invalid root flags, have (0x[0-9a-fA-F]+)`)

	// ② 的原文：corrupt leaf: root=257 block=84230144 slot=1 ino=8512,
	//          unknown incompat flags detected: 0x40000000
	//
	// 注意 slot 与 ino 之间**没有空格**：内核 6.18 实际打印成
	// "slot=114ino=3207"（漏了分隔符）。早期正则写死 "slot=(\d+) ino=" 导致
	// 真机上永远匹配不上、被归成"未命中已知模式"——2026-10-31 真机日志踩过。
	// 这里用 \s* 兼容有空格与无空格两种形态。
	leafFlagRE = regexp.MustCompile(
		`corrupt leaf: root=(\d+) block=\d+ slot=(\d+)\s*ino=(\d+), ` +
			`unknown incompat flags detected: (0x[0-9a-fA-F]+)`)
)

// synoInodeFlag 是群晖 DSM 7 写在 inode item 上的私有标志位（bit 30）。
// 内核把整个 flags 字一起打出来（真实值 0x40000000，还夹着若干合法位），
// 所以判断时不能按字面比字符串，要按位与。
const synoInodeFlag = 0x40000000

// hasSynoInodeFlag 判断内核打印的 flags 字里是否含群晖私有位。
func hasSynoInodeFlag(flagHex string) bool {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(flagHex), "0x"), 16, 64)
	if err != nil {
		return false
	}
	return v&synoInodeFlag != 0
}

// leafFlagHit 一次「叶子里的 inode 带私有标志」命中。
type leafFlagHit struct {
	Root int    // 树根对象号（子卷对应一棵树）
	Slot int    // 叶子内的槽位
	Ino  uint64 // 出问题的 inode 号
	Flag string // 主核不认识的标志
}

// scanLeafFlags 从内核日志里捞出所有 ② 类命中。
func scanLeafFlags(logs []string) []leafFlagHit {
	var hits []leafFlagHit
	for _, l := range logs {
		m := leafFlagRE.FindStringSubmatch(l)
		if len(m) != 5 {
			continue
		}
		root, _ := strconv.Atoi(m[1])
		slot, _ := strconv.Atoi(m[2])
		ino, _ := strconv.ParseUint(m[3], 10, 64)
		hits = append(hits, leafFlagHit{Root: root, Slot: slot, Ino: ino, Flag: m[4]})
	}
	return hits
}

// fsRootProbe 只读试挂文件系统根（subvolid=5），作为「盘本身有没有坏」的旁证。
//
// 判读方式：
//   - 根能挂、目标子卷挂不上 → 超块/树根/块设备都好，问题局限在子卷内部（② 类）
//   - 根也挂不上            → 更上游的问题（① 类，或真损坏）
//
// 返回 1 = 能挂，-1 = 挂不上，0 = 没测成（建临时目录失败等）。
func fsRootProbe(dev string) int {
	mp, err := os.MkdirTemp("", "fn-adopt-root-")
	if err != nil {
		return 0
	}
	defer os.Remove(mp)
	if _, mErr := commander.Run(60*time.Second, "mount", "-o", "ro,subvolid=5", dev, mp); mErr != nil {
		return -1
	}
	commander.Run(30*time.Second, "umount", mp)
	return 1
}

// explainSubvolMountFailure 把"目标子卷挂不上"翻译成原因 + 下一步。
//
// 上一版只会背一句"私有 root flag 0x400000000"，而真正需要的恰恰是分清
// ①（整棵树进不去）和 ②（只是某些 inode 带标）——前者只能换环境，
// 后者可以在旧内核里重建子卷后继续在飞牛上接管。
func explainSubvolMountFailure(dev string, id int, mountErr string) string {
	logs := commander.RecentBtrfsKernelLog(10)
	joined := strings.Join(logs, "\n")
	hits := scanLeafFlags(logs)
	rootState := fsRootProbe(dev)

	var b strings.Builder
	fmt.Fprintf(&b,
		"目标数据子卷（ID %d）挂载失败，接管必然在「设置默认子卷」这步出错，因此不生成计划。\n",
		id)

	switch {
	case len(hits) > 0:
		h := hits[0]
		fmt.Fprintf(&b,
			"原因（已定位）：该子卷里有 inode 带群晖私有标志 %s，"+
				"内核在树 root=%d 的叶子（slot=%d）里读到它，判为 unknown incompat flags，"+
				"整个子卷随即挂载失败——不是只有那个文件打不开。\n"+
				"第一个撞上的是 ino=%d；挂载在第一个坏叶子上就中止了，实际带标的 inode 只会更多。\n",
			h.Flag, h.Root, h.Slot, h.Ino)
		b.WriteString("这不等于坏块：inode 与数据都在盘上，只是标志位主核不认识。\n")
		if rootState == 1 {
			b.WriteString("旁证：同一块盘的文件系统根（subvolid=5）刚才只读挂载成功了，" +
				"说明超块、树根、块设备都正常，问题确实只局限在目标子卷内部。\n")
		}
		b.WriteString("要在飞牛内核上接管，必须先把这些标志清掉，三条路：\n" +
			"  a) 在群晖 DSM7 本机、或 4.15.0-20 这类未 backport tree-checker 加固的旧内核里挂上它；\n" +
			"  b) 在旧内核里把 @syno 的内容复制/重建成一个干净子卷 → 删旧子卷 → 新子卷设为默认；\n" +
			"     复制出来的 inode 是新写的，不带私有标志（本工具救另外几块盘用的就是这条）；\n" +
			"  c) 或者等 v0.6 给飞牛内核的 btrfs.ko 打 tree-checker 补丁，再回来接管。\n")

	case rootFlagRE.MatchString(joined):
		m := rootFlagRE.FindStringSubmatch(joined)
		fmt.Fprintf(&b,
			"原因（已定位）：树根的 root item 带群晖私有 root flag %s，"+
				"内核在 open_ctree 阶段就拒了——这类盘的整棵文件系统树都进不去。\n", m[1])
		b.WriteString("处理办法：先在能识别该标志的环境（群晖 DSM7 / 4.4 内核）里处理这块盘，再回来接管。\n")

	case rootState == -1:
		b.WriteString("原因未在内核日志里命中已知模式，且文件系统根（subvolid=5）也挂不上——" +
			"更像是真损坏或设备不可读，而不是私有标志。请把下面的内核日志整段发出来。\n")

	default:
		b.WriteString("原因未在内核日志里命中已知模式（可能内核不支持该子卷，或日志已被刷掉）。\n" +
			"请把下面的内核日志整段发出来。\n")
	}

	fmt.Fprintf(&b, "盘上的数据与卷名均未被本步骤改动。\nmount 原始报错: %s", mountErr)
	if len(logs) > 0 {
		b.WriteString("\n内核日志（最近 btrfs 相关，时间递增）:\n  " + strings.Join(logs, "\n  "))
	}
	return b.String()
}

// explainFSTreeMountFailure 专治「连 subvolid=5 都挂不上」。
//
// 真机 2026-10-31：vg1001 在旧内核里重建子卷后放回飞牛，open_ctree 直接失败，
// mount 只报 "can't read superblock"（exit 32），看不出任何原因——因为内核日志
// 根本没被带出来。这个函数把 dmesg 里的 btrfs 行捞出来并按已知模式归因。
//
// 注意这与 explainSubvolMountFailure 是两种局面：
//   - 那边：根能挂、子卷挂不上 → 问题局限在子卷内部
//   - 这边：连根都挂不上     → 问题在根树本身（root item 的 flags / 超块 / 真损坏）
func explainFSTreeMountFailure(dev, mountErr string) string {
	logs := commander.RecentBtrfsKernelLog(20)
	joined := strings.Join(logs, "\n")
	hits := scanLeafFlags(logs)
	// 只读旁证：这一步失败的是**读写**挂载。若同一设备只读挂载能成，就说明
	// 超块/树根/设备都健康，唯一差别在 rw 独有的那部分工作（典型就是拆 dead root）。
	rootState := fsRootProbe(dev)

	var b strings.Builder
	b.WriteString("文件系统根（subvolid=5）挂载失败，接管在第一步就无法继续。\n")

	switch {
	case rootFlagRE.MatchString(joined):
		m := rootFlagRE.FindStringSubmatch(joined)
		fmt.Fprintf(&b,
			"原因（已定位）：根树里某个 ROOT_ITEM 带着群晖私有 root flag %s，"+
				"内核在 open_ctree 阶段就读不下去了。\n"+
				"注意这是**根层面**的标志，与子卷里 inode 的标志是两回事：\n"+
				"  · inode flags（0x40000000）只让某个子卷挂不上，可以在旧内核里重建子卷解决；\n"+
				"  · root flags（0x400000000）会让整棵树都进不去，重建子卷也去不掉。\n", m[1])
		b.WriteString("两种可能，按顺序排查：\n" +
			"  1) 刚删过子卷、cleaner 还没把旧 ROOT_ITEM 拆完（删除是异步的，元数据上已消失\n" +
			"     但 root item 还留在根树里，带着旧标志）→ 回旧内核挂上后执行\n" +
			"     `btrfs subvolume sync <挂载点>`，等它真的删完再卸载，然后重试；\n" +
			"  2) 标志确实落在这块盘仍在使用的子卷上 → 只能换环境（DSM7 / 4.4 内核）\n" +
			"     或给飞牛内核打 tree-checker 补丁。\n")

	case len(hits) > 0 && hasSynoInodeFlag(hits[0].Flag):
		h := hits[0]
		fmt.Fprintf(&b,
			"原因（已定位）：树 root=%d 的叶子 slot=%d 处，ino=%d 带群晖私有 inode 标志 %s，"+
				"tree-checker 在遍历时就中止了，open_ctree 随之失败。\n",
			h.Root, h.Slot, h.Ino, h.Flag)
		if rootState == 1 {
			// 真机 2026-10-31 vg1001：就是这一段。删旧子卷后立刻卸载，
			// cleaner 没跑完，根树里留下 refs=0 的 dead root；ro 不碰它所以能挂，
			// rw 挂载会把它拆掉 → 读到带标 inode → 失败。
			fmt.Fprintf(&b,
				"关键旁证：同一块盘用**只读**方式挂 subvolid=5 是成功的，只有**读写**挂载失败。\n"+
					"这说明树 root=%d 已经不在使用中——它是删掉的旧子卷留下的 dead root\n"+
					"（subvolume delete 是异步的：元数据立刻消失，extent 和 ROOT_ITEM 要等 cleaner 收尾）。\n"+
					"只读挂载不会去动 dead root，所以能挂；读写挂载会先把它拆掉，一读就撞上带标 inode。\n",
				h.Root)
			b.WriteString("处理办法（数据一行都不用动，也不用再重建子卷）：\n" +
				"  1) 把这块盘接到 4.15.0-20 这类不校验该标志的旧内核上，**读写**挂载；\n" +
				"  2) 让它把 dead root 真的删完：执行 `btrfs subvolume sync <挂载点>`，\n" +
				"     或反复 `btrfs inspect-internal dump-tree -t 1 <设备> | grep -c 'refs 0'`，\n" +
				"     等这个数变成 0；\n" +
				"  3) 干净 umount，再放回飞牛，rw 挂载即可成功。\n")
			b.WriteString("这也暴露了重建脚本的缺陷：删完旧子卷必须 sync 再卸载，" +
				"否则就会留下这个 dead root（新版 rebuild-subvol.sh 已补上）。\n")
		} else {
			b.WriteString("这与「子卷挂不上」是同一类问题，只是发生得更早。\n" +
				"处理办法：在 4.15.0-20 这类旧内核里把带标子卷重建成干净子卷后再接管。\n")
		}

	case len(hits) > 0:
		// 有未知 inode 标志，但不是群晖那一位——别误导用户去换内核。
		h := hits[0]
		fmt.Fprintf(&b,
			"原因（已定位）：树 root=%d 的叶子 slot=%d 处，ino=%d 带内核不认识的 inode 标志 %s，\n"+
				"但它不含群晖常见的 bit30(0x40000000)。先别急着换内核，可能是别的特性位或真损坏。\n",
			h.Root, h.Slot, h.Ino, h.Flag)
		b.WriteString("把下面的内核日志整段发出来，按实际标志位再判断。\n")

	default:
		if rootState == 1 {
			b.WriteString("内核对同一个设备的**只读**挂载是成功的，只有读写挂载失败，" +
				"但日志里没命中已知模式。\n" +
				"优先怀疑根树里还有没拆完的 dead root（删子卷后没 sync 就卸载）。\n" +
				"回旧内核 rw 挂上执行 `btrfs subvolume sync <挂载点>`，等真删完再卸载，然后重试。\n")
		} else {
			b.WriteString("内核日志里没命中已知模式，可能是超块/树根真损坏，或日志已被刷掉。" +
				"请把下面的日志整段发出来。\n")
		}
	}

	fmt.Fprintf(&b, "盘上的数据与卷名均未被本步骤改动。\nmount 原始报错: %s", mountErr)
	if len(logs) > 0 {
		b.WriteString("\n内核日志（最近 btrfs 相关，时间递增）:\n  " + strings.Join(logs, "\n  "))
	} else {
		b.WriteString("\n（没能读到内核日志：dmesg 需要 root，journalctl -k 也没取到东西）")
	}
	return b.String()
}
