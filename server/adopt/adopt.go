// Package adopt 实现群晖 Basic 盘的零拷贝接管（v0.2 核心）。
//
// 原理（对齐社区验证流程 tid=69135）：数据本身不动，只做
//  1. LVM 改名到飞牛约定：vgN -> trim_<uuid>，逻辑卷（volume_N 或 lv）-> 0
//  2. btrfs 默认子卷切到 @syno（群晖数据子卷，ID 通常 256）
//  3. 挂载到 /volN
//  4. 向飞牛 PostgreSQL trim 库 mount 表注册一行（uuid = trim_<uuid>-0）
//
// 安全设计：
//   - 计划（Plan）与执行（Execute）分离；执行前全量预检
//   - SYNOKIT_DRYRUN=1 或 dryRun 参数时只打印命令，绝不写盘
//   - vgcfgbackup + pg_dump 双备份先行，落盘 /var/lib/fn-syno-adopt/backup/
//   - 多块群晖盘卷组全部重名 vg1，全程以 VG UUID 定位
package adopt

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
)

// StateDir 持久状态根目录（接管记录 adopted.json、备份都在这里）。
var StateDir = "/var/lib/fn-syno-adopt"

// BackupRoot 备份文件根目录。
var BackupRoot = StateDir + "/backup"

// 接管布局类型：LVM 路径（群晖 Basic，vgrename/lvrename）与直建路径（btrfs
// 直接建在 md 阵列上，无 LVM 层，无需改名）。
const (
	PlanKindLVM    = "lvm"
	PlanKindDirect = "direct"
)

// Plan 一次接管的完整计划。
type Plan struct {
	Kind         string   `json:"kind"`   // lvm / direct；v0.5.x 的旧记录无此字段，反序列化为 ""，按 lvm 处理
	VGUuid       string   `json:"vgUuid"` // LVM：卷组 UUID；direct：md 阵列 UUID（接管记录主键）
	OldVG        string   `json:"oldVG"`
	OldLV        string   `json:"oldLV"` // 群晖原始逻辑卷名：volume_7 / lv / volume_1（真机上五花八门，不可写死）
	NewVG        string   `json:"newVG"`
	DMPath       string   `json:"dmPath"`
	MdDev        string   `json:"mdDev,omitempty"`    // direct：底层 md 阵列设备（如 /dev/md2）
	MdUUIDs      []string `json:"mdUuids,omitempty"`  // direct：阵列 UUID（mdadm.conf 持久化与回滚移除用）
	DmDev        string   `json:"dmDev,omitempty"`    // direct：dm-linear 包装设备 /dev/mapper/trim_<uuid>-0
	Converge     bool     `json:"converge,omitempty"` // direct：mount 表已有同 uuid 行，转正模式（复用编号，跳过 INSERT）
	VolID        int      `json:"volId"`
	MountPoint   string   `json:"mountPoint"`
	MUUID        string   `json:"muuid"` // mount 表 uuid：LVM=vgrename 后的 trim_<uuid>-0；direct=dm 包装设备名（同格式）
	SynoSubvolID int      `json:"synoSubvolId"`
	Disk         string   `json:"disk"`
	LVSize       string   `json:"lvSize"`
	Steps        []string `json:"steps"`
	Warnings     []string `json:"warnings"`
}

// MRow mount 表一行。
type MRow struct {
	ID   int
	UUID string
}

// volPointRE 合法的挂载点：/volN 或 /volN/...（N 为 1-2 位数字）。
var volPointRE = regexp.MustCompile(`^/vol\d+$`)

// trimDMName 把 md 阵列 UUID 转成飞牛原生命名风格的 dm 设备名 trim_<uuid>-0。
// 冒号与连字符都替换为下划线（真机教训：只替换 - 会留下冒号，与原生风格不符）。
// 例：5084ea63:60d32535:d0732154:8b167f25 -> trim_5084ea63_60d32535_d0732154_8b167f25-0
func trimDMName(mdUUID string) string {
	return "trim_" + strings.NewReplacer(":", "_", "-", "_").Replace(mdUUID) + "-0"
}

// dmUnitPath 直建卷的开机自动包装 systemd unit 路径。
func dmUnitPath(muuid string) string {
	return "/etc/systemd/system/fn-syno-adopt-dm-" + muuid + ".service"
}

// dmUnitContent 生成开机自动重建 dm-linear 映射并挂载的 systemd unit。
// 幂等：dm 已存在则跳过创建；挂载点已挂则跳过挂载。
// 之所以在 unit 里一并挂载：飞牛自己的开机挂载时序不可控，由我们保证
// /volN 就绪，飞牛存储服务看到挂载好的设备即可正常认领。
func dmUnitContent(mdDev, muuid, mountPoint string) string {
	return fmt.Sprintf(`[Unit]
Description=fn-syno-adopt dm-linear wrapper for %[1]s (Synology direct btrfs)
After=local-fs.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'dmsetup info %[2]s >/dev/null 2>&1 || { s=$(blockdev --getsize %[1]s); echo "0 $s linear %[1]s 0" | dmsetup create %[2]s; }'
ExecStartPost=/bin/sh -c 'mountpoint -q %[3]s || mount -o rw,noatime,space_cache=v2 /dev/mapper/%[2]s %[3]s'

[Install]
WantedBy=multi-user.target
`, mdDev, muuid, mountPoint)
}

// genTrimVG 生成新卷组名 trim_<uuid>（小写、下划线，风格对齐飞牛原生）。
func genTrimVG() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // 系统熵源不可用时无法安全继续
	}
	return "trim_" + strings.ReplaceAll(hex.EncodeToString(b)[:32], "-", "")
}

// ProcMounts 解析 /proc/self/mounts，返回挂载点列表（Go 实现，零命令调用）。
func ProcMounts() []string {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	var mps []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			mp := strings.ReplaceAll(f[1], "\\040", " ")
			mps = append(mps, mp)
		}
	}
	return mps
}

// MountTable 查询飞牛 mount 表（只读）。
func MountTable() ([]MRow, error) {
	args := []string{"-d", "trim", "-P", "pager=off", "-A", "-t", "-c",
		"SELECT id,uuid FROM mount ORDER BY id"}
	r, err := commander.RunPostgres(30*time.Second, "psql", args...)
	if err != nil {
		return nil, fmt.Errorf("%w\n可手工执行: %s", err, commander.ManualPostgres("psql", args...))
	}
	var rows []MRow
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(f) != 2 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		rows = append(rows, MRow{ID: id, UUID: strings.TrimSpace(f[1])})
	}
	return rows, nil
}

// CrcLoaded 检查内核是否已加载 crc32c（btrfs 校验依赖，飞牛环境最常见坑）。
func CrcLoaded() bool {
	b, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "crc32c ") {
			return true
		}
	}
	return false
}

// NextVolID 计算下一个可用的存储空间编号：
// 同时满足 /volN 目录不存在、未挂载、mount 表无 id=N。
func NextVolID(mps []string, rows []MRow) (int, []string) {
	var warnings []string
	usedIDs := map[int]bool{}
	for _, mp := range mps {
		if volPointRE.MatchString(mp) {
			if n, err := strconv.Atoi(mp[4:]); err == nil {
				usedIDs[n] = true
			}
		}
	}
	for _, r := range rows {
		usedIDs[r.ID] = true
	}
	for n := 1; n <= 99; n++ {
		if !usedIDs[n] {
			if _, err := os.Stat(fmt.Sprintf("/vol%d", n)); err == nil {
				warnings = append(warnings, fmt.Sprintf("/vol%d 目录已存在但未挂载也未注册，编号顺延", n))
				continue
			}
			return n, warnings
		}
	}
	return 0, append(warnings, "无可用存储空间编号（/vol1..vol99 均被占用）")
}

// RenumberPlans 消除同一批接管计划之间的挂载点/存储编号冲突。
//
// 每块盘的 BuildPlan 都独立查 mount 表 + /proc/mounts 分配编号，而这两者在
// 「生成计划」阶段不会变——于是多盘一次性生成时全被分到同一个 /volN
// （真机 2026-10-31：同时选 sdg 与 sdb，两张计划卡片都是 /vol6）。
//
// 执行时每块盘会重新 BuildPlan，实际编号是对的（前一块注册后表里就有了新行），
// 所以这不会真的把两块盘挂到一起；但卡片上写得一模一样，用户核对计划时完全
// 看不出先后落点，属于必须修的误导。
func RenumberPlans(plans []*Plan) {
	if len(plans) < 2 {
		return
	}
	taken := map[int]bool{}
	for _, mp := range ProcMounts() {
		if !volPointRE.MatchString(mp) {
			continue
		}
		if n, err := strconv.Atoi(mp[4:]); err == nil {
			taken[n] = true
		}
	}
	if rows, err := MountTable(); err == nil {
		for _, r := range rows {
			taken[r.ID] = true
		}
	}
	for _, p := range plans {
		if p == nil || p.VolID == 0 {
			continue
		}
		id := p.VolID
		for taken[id] {
			id++
		}
		taken[id] = true
		if id == p.VolID {
			continue
		}
		oldMP := p.MountPoint
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"同批接管时 %s 已被前一块盘占用，本盘顺延到 /vol%d（实际执行时会按当时的占用情况再确认一次）",
			oldMP, id))
		p.VolID = id
		p.MountPoint = fmt.Sprintf("/vol%d", id)
		// Steps 里写死了 "mkdir /volN"、"mount ... /volN"，编号一变必须同步，
		// 否则计划卡片上每一步还在说旧挂载点，等于没改。
		for i, s := range p.Steps {
			p.Steps[i] = strings.ReplaceAll(s, oldMP, p.MountPoint)
		}
	}
}

// locateSynoSubvol 定位设备上 @syno 数据子卷的 ID（只读）。
// LVM 与直建两条接管路径共用：找不到 @syno 一律终止，绝不猜 ID。
//
// 真机实锤（md2，2026-10-30）：数据根子卷名可能是怪名 @//syno/（含斜杠，
// 群晖命名残留），且盘上可能同时存在干净名与怪名两个候选。选择策略：
//  1. 剥掉分隔符后 == "@syno" 的都算候选（IsSynoSubvolName）；
//  2. 当前默认子卷（get-default）指着的候选优先——那才是"正在服役"的正主，
//     避免把默认切到另一个旧副本上；
//  3. 无 default 依据时干净名 @syno 优先，其次唯一怪名候选；
//  4. 多候选且都无法定夺 → 报错列出，人工确认，绝不猜。
func locateSynoSubvol(dev string) (int, error) {
	// 统一入口：自动挂载失败时回退复用设备已有挂载点 / dm 包装挂载点。
	allSubs, _, subErr := commander.BtrfsSubvolsForDev(dev)
	var cands []commander.BtrfsSubvol
	for _, sv := range allSubs {
		if commander.IsSynoSubvolName(sv.Path) {
			cands = append(cands, sv)
		}
	}
	if len(cands) == 0 {
		extra := ""
		if subErr != nil {
			extra = "（读取子卷失败: " + subErr.Error() + "）"
		} else if len(allSubs) > 0 {
			names := []string{}
			for _, sv := range allSubs {
				names = append(names, sv.Path)
			}
			extra = "（该卷上的子卷: " + strings.Join(names, "、") + "）"
		} else {
			extra = "（该卷上没有任何子卷，卷可能未激活——可先在接管页点『激活卷组』）"
		}
		return 0, fmt.Errorf("未找到 @syno 数据子卷，接管终止%s", extra)
	}

	// 默认子卷正主优先：default 指着的候选就是现在实际服役的数据根
	if mp := firstMountPoint(dev); mp != "" {
		if defID, err := commander.BtrfsDefaultSubvolID(mp); err == nil {
			for _, sv := range cands {
				if sv.ID == defID {
					return sv.ID, nil
				}
			}
		}
	}
	// 干净名优先，其次唯一候选
	for _, sv := range cands {
		if sv.Path == "@syno" {
			return sv.ID, nil
		}
	}
	if len(cands) == 1 {
		return cands[0].ID, nil
	}
	names := []string{}
	for _, sv := range cands {
		names = append(names, fmt.Sprintf("%s(ID %d)", sv.Path, sv.ID))
	}
	return 0, fmt.Errorf(
		"找到多个 @syno 数据根候选（%s），且默认子卷都不指向它们——"+
			"为避免把默认子卷切到旧副本上，接管终止；请人工确认哪个是现役数据根", strings.Join(names, "、"))
}

// verifySubvolMountable 只读挂载一次目标子卷，确认内核真的认它。
//
// 为什么要在**生成计划**阶段做这件事（真机 2026-10-31 两次翻车后补的）：
// 群晖 DSM7 给共享文件夹子卷打了私有 root flag（0x400000000），主线内核的
// tree-checker 会拒读它的 root item。但 FS 根（subvolid=5）是能挂的，子卷列表
// 也能列（用户态 btrfs-progs 不校验），所以探测阶段一切正常——直到执行到
// 「btrfs subvolume set-default」才炸，报 Input/output error 或 Invalid argument。
// 那时卷组名、逻辑卷名都已经改过了，盘卡在半接管状态，界面上既不在候选列表
// （名字不再是 vgN/volume_N）也不在接管记录（还没写），用户只能干瞪眼。
//
// 提前试挂一次（ro，不写盘、不改任何元数据），把这种"必败"在计划阶段就拦掉。
func verifySubvolMountable(dev string, id int) error {
	mp, err := os.MkdirTemp("", "fn-adopt-chk-")
	if err != nil {
		return nil // 建不了临时目录就跳过校验，不因为校验本身阻断流程
	}
	defer os.Remove(mp)

	res, mErr := commander.Run(60*time.Second, "mount", "-o",
		"ro,subvolid="+strconv.Itoa(id), dev, mp)
	if mErr != nil {
		// 一定要把 mount 的 stderr 带出来。之前只留了 Go 的 "exit status 32"，
		// 用户和我都看不出内核到底说了什么（can't read superblock / Invalid argument
		// / Input/output error 差别很大，直接决定下一步怎么做）。
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = mErr.Error()
		}
		// 归因与建议统一交给 diagnose.go：两类私有标志（root flag / inode flag）
		// 的处理办法完全不同，不能在这里背一句固定的"私有 root flag"。
		return errors.New(explainSubvolMountFailure(dev, id, detail))
	}
	commander.Run(30*time.Second, "umount", mp)
	return nil
}

// firstMountPoint 返回设备当前第一个可用挂载点（没有则空串）。
func firstMountPoint(dev string) string {
	mps := commander.MountPointsOf(dev)
	if len(mps) == 0 {
		return ""
	}
	return mps[0]
}

// allocVol 为计划分配存储空间编号与挂载点，并做占用预检（只读）。
func allocVol(p *Plan) error {
	rows, err := MountTable()
	if err != nil {
		p.Warnings = append(p.Warnings, "mount 表查询失败（"+err.Error()+"），编号仅按挂载点分配，执行时会再次校验")
	}
	p.VolID, p.Warnings = NextVolID(ProcMounts(), rows)
	if p.VolID == 0 {
		return fmt.Errorf("无法分配存储空间编号: %s", strings.Join(p.Warnings, "；"))
	}
	p.MountPoint = fmt.Sprintf("/vol%d", p.VolID)

	// 冲突预检
	for _, mp := range ProcMounts() {
		if mp == p.MountPoint {
			return fmt.Errorf("%s 已被占用", p.MountPoint)
		}
	}
	if _, err := os.Stat(p.MountPoint); err == nil {
		p.Warnings = append(p.Warnings, p.MountPoint+" 目录已存在（未挂载），执行时将复用")
	}
	return nil
}

// BuildPlan 为指定 VG UUID 生成接管计划（全程只读）。
func BuildPlan(vgUuid string) (*Plan, error) {
	if vgUuid == "" {
		return nil, fmt.Errorf("缺少卷组 UUID")
	}
	cands, _, err := commander.DetectSynologyDisks()
	if err != nil {
		return nil, err
	}
	var c *commander.SynoCandidate
	for i := range cands {
		if cands[i].VGUuid == vgUuid {
			c = &cands[i]
			break
		}
	}
	if c == nil {
		return nil, fmt.Errorf("未找到卷组 UUID=%s 的候选盘，请刷新检测", vgUuid)
	}
	if !c.Adoptable {
		return nil, fmt.Errorf("该盘不满足接管条件（%s/%s fs=%s）： %s",
			c.VG, c.LV, c.FS, strings.Join(c.Notes, "；"))
	}
	if c.DMPath == "" {
		return nil, fmt.Errorf("设备映射路径缺失")
	}

	p := &Plan{
		Kind:     PlanKindLVM,
		VGUuid:   c.VGUuid,
		OldVG:    c.VG,
		OldLV:    c.LV, // 真实逻辑卷名，lvrename 与回滚都以它为准
		DMPath:   c.DMPath,
		Disk:     c.Disk,
		LVSize:   c.LVSize,
		Warnings: []string{},
	}
	p.NewVG = genTrimVG()
	p.MUUID = p.NewVG + "-0"

	// @syno 子卷定位（找不到直接终止）
	synoID, err := locateSynoSubvol(c.DMPath)
	if err != nil {
		return nil, err
	}
	p.SynoSubvolID = synoID

	// 生成计划阶段就验证目标子卷可挂载：群晖私有 root flag 会让接管在
	// set-default 那步必败，而那时卷名已改，盘会卡成无法回滚的半接管状态。
	// 错误里带上盘名：多块盘同时验证时，光看子卷 ID 分不清是谁失败了。
	if err := verifySubvolMountable(c.DMPath, synoID); err != nil {
		return nil, fmt.Errorf("%s（%s/%s）%w", c.Disk, c.VG, c.LV, err)
	}

	if err := allocVol(p); err != nil {
		return nil, err
	}

	p.Steps = []string{
		"modprobe crc32c（确保 btrfs 校验模块可用）",
		"vgcfgbackup 备份卷组配置（按卷组名，备份后核对 UUID 确属本盘） -> " + BackupRoot + "/<时间戳>/",
		"vgrename " + shortUUID(p.VGUuid) + "... -> " + p.NewVG + "（按 UUID 定位，重名免疫）",
		"lvrename " + p.NewVG + " " + p.OldLV + " 0（原始卷名，非写死的 volume_1）",
		"vgchange -ay " + p.NewVG,
		// 挂载必须写**改名后**的设备名：vgrename 之后 /dev/mapper/vg1002-lv 已经
		// 不存在了。之前这里照抄了探测阶段的旧路径，计划卡片与实际执行对不上，
		// 用户核对时还以为命令写错了。
		"mount " + "/dev/mapper/" + p.MUUID + " /mnt/_adopt_tmp（临时挂载设默认子卷）",
		"btrfs subvolume set-default " + strconv.Itoa(p.SynoSubvolID) + " /mnt/_adopt_tmp（@syno）",
		"umount /mnt/_adopt_tmp",
		"mkdir " + p.MountPoint,
		"mount -o rw,noatime,trimacl,space_cache=v2 " + "/dev/mapper/" + p.MUUID + " " + p.MountPoint,
		"pg_dump 备份 mount 表 -> " + BackupRoot + "/<时间戳>/mount_before.sql",
		"INSERT INTO mount (id=" + strconv.Itoa(p.VolID) + ", uuid=" + p.MUUID + ")",
		"mdadm.conf 持久化：追加 ARRAY 行（带 # fn-syno-adopt 标记），保证重启后阵列自动组装",
		"验证：挂载点生效 + mount 表含新记录",
	}
	return p, nil
}

// BuildPlanDirect 为直建 btrfs（无 LVM 层，btrfs 直接建在 md 阵列上）生成接管计划。
//
// 与 LVM 路径的差异：没有卷组/逻辑卷可改名，vgrename、lvrename、vgcfgbackup
// 整段跳过。核心是 **dm-linear 透明包装**：把 /dev/mdN 包装成飞牛原生命名
// /dev/mapper/trim_<uuid>-0 再注册——真机验证（2026-10-30）飞牛只认这种
// /dev/mapper/trim_* 设备，直接注册 /dev/mdN 会被判"已移除"。
// dm-linear 是 LVM 底层的实现方式，零拷贝、对数据完全透明。
//
// 两个持久化配套：
//   - mdadm.conf：保证重启后 md 阵列自动组装
//   - systemd unit：开机自动重建 dm 映射并挂载 /volN（dm 映射本身不持久）
//
// 转正模式：mount 表已有同 uuid 行（如手工试验遗留 / 重复接管）时复用该编号，
// 只补齐 dm 映射 / 开机 unit / 挂载 / 接管记录，跳过 INSERT，幂等可重跑。
func BuildPlanDirect(mdDev string) (*Plan, error) {
	if mdDev == "" {
		return nil, fmt.Errorf("缺少 md 阵列设备路径")
	}
	cands, _, err := commander.DetectSynologyDisks()
	if err != nil {
		return nil, err
	}
	var c *commander.SynoCandidate
	for i := range cands {
		if cands[i].Kind == commander.KindDirect && cands[i].MdDev == mdDev {
			c = &cands[i]
			break
		}
	}
	if c == nil {
		return nil, fmt.Errorf("未找到设备 %s 的直建 btrfs 候选盘，请刷新检测", mdDev)
	}
	if !c.Adoptable {
		return nil, fmt.Errorf("该盘不满足接管条件（%s fs=%s）： %s",
			c.MdDev, c.FS, strings.Join(c.Notes, "；"))
	}

	d := commander.MdDetailOf(mdDev)
	if d.UUID == "" {
		return nil, fmt.Errorf("读取 %s 的 mdadm UUID 失败，无法生成接管计划", mdDev)
	}

	p := &Plan{
		Kind:     PlanKindDirect,
		VGUuid:   d.UUID, // direct 记录主键 = 阵列 UUID（唯一且稳定）
		DMPath:   mdDev,
		MdDev:    mdDev,
		MdUUIDs:  []string{d.UUID},
		Disk:     c.Disk,
		LVSize:   c.LVSize,
		Warnings: []string{},
	}
	p.MUUID = trimDMName(d.UUID)
	p.DmDev = "/dev/mapper/" + p.MUUID

	// @syno 子卷定位（找不到直接终止）
	synoID, err := locateSynoSubvol(mdDev)
	if err != nil {
		return nil, err
	}
	p.SynoSubvolID = synoID

	// 同 LVM 路径：直建布局一样会在 set-default 上撞群晖私有 root flag，
	// 提前试挂一次，把必败挡在计划阶段。
	if err := verifySubvolMountable(mdDev, synoID); err != nil {
		return nil, fmt.Errorf("%s（%s）%w", c.Disk, mdDev, err)
	}

	// 转正检测：mount 表已有同 uuid 行（手工 dm-linear 试验遗留 / 重复接管）
	var converged *MRow
	if rows, rowsErr := MountTable(); rowsErr == nil {
		for i := range rows {
			if rows[i].UUID == p.MUUID {
				converged = &rows[i]
				break
			}
		}
	}
	if converged != nil {
		p.Converge = true
		p.VolID = converged.ID
		p.MountPoint = fmt.Sprintf("/vol%d", p.VolID)
		p.Warnings = append(p.Warnings,
			fmt.Sprintf("检测到 mount 表已有 uuid=%s 的注册行（id=%d），本次为**转正模式**："+
				"复用该编号与挂载点，跳过 INSERT，仅补齐 dm 映射 / 开机自启 unit / 挂载 / 接管记录",
				p.MUUID, p.VolID))
	} else {
		if err := allocVol(p); err != nil {
			return nil, err
		}
	}
	p.Warnings = append(p.Warnings,
		"直建布局无 LVM 层：用 dm-linear 把 "+mdDev+" 透明包装成 "+p.DmDev+
			"（LVM 底层同款机制，不改动盘上任何数据与卷名），并以飞牛原生命名注册——"+
			"真机已验证存储管理器正常认账。重启后由 systemd unit 自动重建映射并挂载")

	regStep := "INSERT INTO mount (id=" + strconv.Itoa(p.VolID) + ", uuid=" + p.MUUID + ", pool_type=0)"
	if p.Converge {
		regStep = "（转正模式：复用已有注册行 id=" + strconv.Itoa(p.VolID) + "，跳过 INSERT）"
	}
	p.Steps = []string{
		"modprobe crc32c（确保 btrfs 校验模块可用）",
		"直建布局无 LVM 层：跳过 vgcfgbackup / vgrename / lvrename",
		"dmsetup create " + p.MUUID + " --table \"0 <扇区> linear " + mdDev + " 0\"（透明包装，已存在则跳过）",
		"写入并启用开机自动包装 unit：" + dmUnitPath(p.MUUID),
		"mount -o subvolid=5 " + p.DmDev + " /mnt/_adopt_tmp（临时挂载 FS tree）",
		"btrfs subvolume set-default " + strconv.Itoa(p.SynoSubvolID) + " /mnt/_adopt_tmp（@syno）",
		"umount /mnt/_adopt_tmp",
		"mkdir " + p.MountPoint,
		"mount -o rw,noatime,trimacl,space_cache=v2 " + p.DmDev + " " + p.MountPoint + "（已挂载则跳过）",
		"pg_dump 备份 mount 表 -> " + BackupRoot + "/<时间戳>/mount_before.sql",
		regStep,
		"mdadm.conf 持久化：追加 ARRAY 行（带 # fn-syno-adopt 标记），保证重启后阵列按 " + mdDev + " 组装",
		"验证：挂载点生效 + mount 表含 id=" + strconv.Itoa(p.VolID),
	}
	return p, nil
}

// umountTolerant 幂等卸载：目标本来就没挂载 → 记"跳过"不执行；busy →
// 降级 umount -l 惰性卸载。与 unmountAllFor 同一语义，供接管流程的
// 临时挂载（/mnt/_adopt_tmp）使用，保证中断后重跑不卡死在残留状态。
func umountTolerant(stepName, target string, dryRun bool, rec *jobs.Recorder) error {
	sl := rec.Add(stepName, "umount "+target)
	if dryRun {
		sl.Done("pending", "演练")
		return nil
	}
	if !commander.IsMountpoint(target) {
		sl.Done("ok", "未挂载，跳过")
		return nil
	}
	r, err := commander.Run(60*time.Second, "umount", target)
	if err == nil {
		sl.Done("ok", summarize(r.Stdout, ""))
		return nil
	}
	sl.Done("pending", "常规卸载失败（"+summarize(r.Stderr, "")+"），降级 umount -l")
	r2, err2 := commander.Run(60*time.Second, "umount", "-l", target)
	if err2 != nil {
		sl.Done("fail", summarize(r2.Stderr, err2.Error()))
		return err2
	}
	sl.Done("ok", "已惰性卸载")
	return nil
}

// ensureTrimacl 给一个已挂载的卷现场补上 trimacl 选项。
//
// 为什么需要它：trimacl 由飞牛自研的 trimafs2 模块提供，也是飞牛「应用 → 访问权限」
// 能给该卷加目录（影视/相册/下载器绑定目录）的**前提**。缺这个选项的卷文件读写
// 一切正常，唯独授权接口返回 "Request failed, please try again later"，用户完全
// 看不出根因。真机 2026-10-31 的 /vol2 /vol4 就是当初手工 mount 时漏了参数。
//
// 做法：先 modprobe 把模块点起来，再 remount 追加选项。util-linux 的 remount
// 会把 /proc/mounts 里的现有选项合并进来，所以只写 remount,trimacl 就够了，
// 不用手工拼全量选项（拼错了反而可能把 rw 变 ro）。
func ensureTrimacl(mountPoint string) error {
	_, _ = commander.Run(60*time.Second, "modprobe", "trimafs2")
	r, err := commander.Run(60*time.Second, "mount", "-o", "remount,trimacl", mountPoint)
	if err != nil {
		// 模块刚加载可能还没就绪：再探一次后原样重试
		_, _ = commander.Run(60*time.Second, "modprobe", "trimafs2")
		r, err = commander.Run(60*time.Second, "mount", "-o", "remount,trimacl", mountPoint)
		if err != nil {
			return fmt.Errorf("%s", summarize(r.Stderr, err.Error()))
		}
	}
	// 不轻信返回值：确认选项真的出现了，否则等于没补
	if !commander.MountHasOption(mountPoint, "trimacl") {
		return fmt.Errorf("remount 返回成功但 /proc/mounts 里仍无 trimacl")
	}
	return nil
}

// deviceFsUUID 读设备上的 btrfs 文件系统 UUID（空串 = 读不到/不是 btrfs）。
func deviceFsUUID(dev string) string {
	if dev == "" {
		return ""
	}
	r, err := commander.Run(15*time.Second, "blkid", "-s", "UUID", "-o", "value", dev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// waitDeviceFsUUID 确认 dm 设备既已就绪、又确实是**同一块盘**，然后才允许继续。
//
// 真机事故（2026-10-31）：vgrename + lvrename 之后立刻 mount，报
// "can't read superblock on /dev/mapper/trim_<uuid>-0"。原因是改名会让 LVM 删掉
// 旧 dm 设备、新建一个，minor 号可能被复用，而 /dev/mapper/trim_*-0 这个符号链接
// 由 udev 异步建立——改名后马上 mount 可能拿到一个过时或指向别人的 dm。
//
// 后果不止于"挂载失败"：紧接着的 btrfs subvolume set-default 会写在**拿到的那个
// 设备**上。若那是另一块已接管的盘，就会改掉别人的默认子卷。所以必须在任何
// 写操作之前，用 btrfs UUID 把设备身份钉死。
//
// wantUUID 为空时（改名前的设备读不到，例如异常重跑）只做 settle 不做比对。
func waitDeviceFsUUID(dev, wantUUID string) error {
	// 等 udev 把改名的设备事件处理完，最多 15 秒
	commander.Run(20*time.Second, "udevadm", "settle", "--timeout=15")
	deadline := time.Now().Add(10 * time.Second)
	got := ""
	for {
		got = deviceFsUUID(dev)
		if wantUUID != "" && got == wantUUID {
			return nil
		}
		// 拿不到改名前的 UUID 时退一步：至少要确认这个设备上真的有 btrfs，
		// 否则同样说明符号链接还没就绪或指错了地方。
		if wantUUID == "" && got != "" {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if got == "" {
		return fmt.Errorf("%s 上没有可读的 btrfs 文件系统（blkid 读不到 UUID）。\n"+
			"改名后设备符号链接可能还没就绪，或指向了没有文件系统的设备。\n"+
			"已在任何写操作之前中止——盘上数据与卷组名未被修改。", dev)
	}
	return fmt.Errorf("%s 身份校验失败：期望 btrfs UUID %s，实际读到 %q。\n"+
		"改名后设备可能尚未切换完成，或 /dev/mapper 指向了别的卷。\n"+
		"已在任何写操作之前中止——盘上数据与卷组名可回滚，未被修改。",
		dev, wantUUID, got)
}

// Execute 按计划执行接管。dryRun=true 时逐步"演练"（只打印）。
// 每步通过 Recorder 记录日志；任一步失败立即中止并返回错误。
func Execute(p *Plan, dryRun bool, rec *jobs.Recorder) error {
	step := func(name, command, status, detail string) {
		rec.Add(name, command).Done(status, detail)
	}
	runner := func(stepName, name string, args ...string) error {
		sl := rec.Add(stepName, name+" "+strings.Join(args, " "))
		r, err := commander.Run(120*time.Second, name, args...)
		if err != nil {
			sl.Done("fail", summarize(r.Stderr, err.Error()))
			return err
		}
		sl.Done("ok", summarize(r.Stdout, ""))
		return nil
	}

	// —— 预检（非 mutating）——
	// 目录已存在本身不算失败（allocVol 已提示复用；转正模式下挂载点本来就在）；
	// 只有被**其它设备**占用挂载才中止。被我们自己的 dm 设备挂着（转正模式）放行。
	if !dryRun && commander.IsMountpoint(p.MountPoint) {
		ours := false
		for _, mp := range commander.MountPointsOf("/dev/mapper/" + p.MUUID) {
			if mp == p.MountPoint {
				ours = true
				break
			}
		}
		if !ours {
			return fmt.Errorf("预检失败: %s 已被其它设备挂载", p.MountPoint)
		}
	}

	// 1. crc32c
	if CrcLoaded() {
		step("加载 crc32c 模块", "(内核已加载 crc32c)", "ok", "跳过")
	} else {
		if dryRun {
			step("加载 crc32c 模块", "modprobe crc32c", "pending", "演练")
		} else if err := runner("加载 crc32c 模块", "modprobe", "crc32c"); err != nil {
			return fmt.Errorf("crc32c 模块加载失败（btrfs 挂载将失败）: %w", err)
		}
		if !dryRun {
			// 持久化：开机自动加载
			dir := "/etc/modules-load.d"
			if mkErr := os.MkdirAll(dir, 0o755); mkErr == nil {
				if wErr := os.WriteFile(dir+"/fn-syno-adopt.conf", []byte("crc32c\n"), 0o644); wErr == nil {
					step("crc32c 开机自动加载", "写 "+dir+"/fn-syno-adopt.conf", "ok", "已写入")
				}
			}
		}
	}

	// 2. 备份目录（程序内建目录，不走 shell）
	ts := time.Now().Format("20060102_150405")
	backupDir := filepath.Join(BackupRoot, ts)
	if !dryRun {
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			return fmt.Errorf("创建备份目录失败: %w", err)
		}
		step("创建备份目录", "mkdir -p "+backupDir, "ok", backupDir)
	}

	// 3-6. LVM 专属步骤：vgcfgbackup / vgrename / lvrename / vgchange。
	// 直建布局（direct）没有 LVM 层，整段跳过，改走 dm-linear 透明包装。
	var dm string
	// 改名/重建 dm 会换掉设备节点，先记下这块盘的 btrfs UUID，改名后据此验明正身
	wantFsUUID := ""
	if p.Kind == PlanKindDirect {
		p.DmDev = "/dev/mapper/" + p.MUUID
		dm = p.DmDev
		wantFsUUID = deviceFsUUID(p.MdDev)

		// 3'. dm-linear 透明包装（幂等）：把 /dev/mdN 包装成飞牛原生命名。
		// 真机验证（2026-10-30）：飞牛只认 /dev/mapper/trim_* 设备，
		// 直接注册 /dev/mdN 会被判"已移除"。
		if dryRun {
			step("创建 dm-linear 包装", "dmsetup create "+p.MUUID+" --table \"0 <扇区> linear "+p.MdDev+" 0\"", "pending",
				"演练（映射已存在则跳过）")
			step("写入开机自动包装 unit", "写 "+dmUnitPath(p.MUUID)+" + systemctl enable", "pending", "演练")
		} else {
			sl := rec.Add("创建 dm-linear 包装", "dmsetup create "+p.MUUID+" --table \"0 <扇区> linear "+p.MdDev+" 0\"")
			if _, ierr := commander.Run(15*time.Second, "dmsetup", "info", p.MUUID); ierr == nil {
				sl.Done("ok", "映射已存在，跳过创建")
			} else {
				// 扇区数用 lsblk -b（字节）换算，避免额外加白 blockdev
				r, serr := commander.Run(15*time.Second, "lsblk", "-dnb", "-o", "SIZE", p.MdDev)
				if serr != nil {
					sl.Done("fail", "读取容量失败: "+summarize(r.Stderr, serr.Error()))
					return fmt.Errorf("读取 %s 容量失败: %w", p.MdDev, serr)
				}
				n, perr := strconv.ParseInt(strings.TrimSpace(r.Stdout), 10, 64)
				if perr != nil || n <= 0 {
					sl.Done("fail", "容量解析失败: "+r.Stdout)
					return fmt.Errorf("解析 %s 容量失败: %q", p.MdDev, r.Stdout)
				}
				sectors := n / 512
				r2, cerr := commander.Run(30*time.Second, "dmsetup", "create", p.MUUID,
					"--table", fmt.Sprintf("0 %d linear %s 0", sectors, p.MdDev))
				if cerr != nil {
					sl.Done("fail", summarize(r2.Stderr, cerr.Error()))
					return fmt.Errorf("创建 dm-linear 映射 %s 失败: %w", p.MUUID, cerr)
				}
				sl.Done("ok", fmt.Sprintf("已创建（%d 扇区，透明映射 %s，不改动任何数据）", sectors, p.MdDev))
			}

			// systemd unit：开机自动重建 dm 映射并挂载 /volN（dm 映射不持久）
			unit := dmUnitContent(p.MdDev, p.MUUID, p.MountPoint)
			if wErr := os.WriteFile(dmUnitPath(p.MUUID), []byte(unit), 0o644); wErr != nil {
				return fmt.Errorf("写入开机自动包装 unit 失败: %w", wErr)
			}
			if err := runner("加载 unit 配置", "systemctl", "daemon-reload"); err != nil {
				return err
			}
			if err := runner("启用开机自动包装", "systemctl", "enable", "fn-syno-adopt-dm-"+p.MUUID+".service"); err != nil {
				return err
			}
		}
		step("LVM 改名", "(直建布局无 LVM 层，跳过)", "ok",
			"设备经 dm-linear 包装为 "+dm+"（飞牛原生命名），盘上数据与卷名零改动")
	} else {
		// 3. vgcfgbackup
		//
		// 三个坑：
		//   a. **vgcfgbackup 不接受 UUID，只认真实卷组名**——传 UUID 会直接
		//      `Volume group "xxx" not found`。真机 2026-10-31 实测：sdf(vg2) 接管
		//      因此这一步整条失败，那块盘是在没有任何 LVM 元数据备份的情况下转过去的。
		//   b. 按名字定位在多块群晖盘（全都叫 vg1）时，理论上会备份到别的盘。
		//      实际不会：LVM 不允许两个同名 VG 同时激活，接管时本盘 VG 必然 active
		//      且名字唯一，按名指向的就是它。
		//   c. 文件名若只用 vg1.bak，同秒接管多块盘会互相覆盖 —— 文件名仍带 UUID 短串。
		//
		// 按名字备份之后**必须核对**：读回 .bak 确认里面真有本次卷组的 UUID，
		// 才能证明备份的是目标盘而不是同名邻居。沉默的假成功比失败更危险。
		//
		// 备份失败降级为警告而非中断：vgrename/lvrename 本身可逆，且后面还有
		// pg_dump 兜底；但在日志里必须醒目标注，让用户知道这一步没有备份。
		vgcfg := filepath.Join(backupDir, p.OldVG+"-"+shortUUID(p.VGUuid)+".bak")
		if dryRun {
			step("备份卷组配置", "vgcfgbackup -f "+vgcfg+" "+p.OldVG, "pending", "演练")
		} else if err := runner("备份卷组配置", "vgcfgbackup", "-f", vgcfg, p.OldVG); err != nil {
			step("备份卷组配置（警告）", "vgcfgbackup -f "+vgcfg+" "+p.OldVG, "warn",
				"⚠ 卷组配置备份失败，本次接管没有 LVM 元数据备份！建议手工执行：vgcfgbackup -f "+
					vgcfg+" "+p.OldVG+" ；流程继续（vgrename 可逆），如不放心中止后手工处理。")
		} else if !fileHasStr(vgcfg, p.VGUuid) {
			step("核对卷组备份（警告）", "grep "+p.VGUuid+" "+vgcfg, "warn",
				"⚠ vgcfgbackup 返回成功，但 "+vgcfg+" 里找不到本次卷组 UUID "+p.VGUuid+
					"——备份到的可能是同名邻居盘，等于没有备份。流程继续（vgrename 可逆）。")
		} else {
			step("核对卷组备份", "grep "+p.VGUuid+" "+vgcfg, "ok", "已备份并核对：卷组 UUID 存在于 "+vgcfg)
		}

		// 4. vgrename（按 UUID 定位，多块 vg1 重名免疫）
		// 改名之前先记住这块盘的 btrfs UUID：改名后 dm 设备会重建，
		// 只有 UUID 能证明我们挂的还是同一块盘。
		wantFsUUID = deviceFsUUID("/dev/mapper/" + p.OldVG + "-" + p.OldLV)
		if dryRun {
			step("重命名卷组", "vgrename "+p.VGUuid+" "+p.NewVG, "pending", "演练")
		} else if err := runner("重命名卷组", "vgrename", p.VGUuid, p.NewVG); err != nil {
			return err
		}

		// 5. lvrename
		if dryRun {
			step("重命名逻辑卷", "lvrename "+p.NewVG+" "+p.OldLV+" 0", "pending", "演练")
		} else if err := runner("重命名逻辑卷", "lvrename", p.NewVG, p.OldLV, "0"); err != nil {
			return err
		}

		// 6. 激活
		if dryRun {
			step("激活卷组", "vgchange -ay "+p.NewVG, "pending", "演练")
		} else if err := runner("激活卷组", "vgchange", "-ay", p.NewVG); err != nil {
			return err
		}

		dm = "/dev/mapper/" + p.NewVG + "-0"
	}

	// 6.5 设备身份闸门 —— 必须在任何写操作（尤其 set-default）之前。
	// 改名会重建 dm 设备，minor 号可能被复用；不等 udev 就挂，可能挂到别的盘上，
	// 随后的 set-default 就会改错盘。用改名前的 btrfs UUID 验明正身。
	if !dryRun {
		sl := rec.Add("校验设备身份", "udevadm settle && blkid -s UUID -o value "+dm)
		if werr := waitDeviceFsUUID(dm, wantFsUUID); werr != nil {
			sl.Done("fail", werr.Error())
			return werr
		}
		if wantFsUUID == "" {
			sl.Done("ok", "已 settle，设备上确认有 btrfs（改名前 UUID 未取到，跳过比对）")
		} else {
			sl.Done("ok", "btrfs UUID "+wantFsUUID+"，与改名前的盘一致")
		}
	}

	// 7. 临时挂载设默认子卷
	// 必须挂 subvolid=5（FS tree）：群晖把 default 设成 @syno，挂默认子卷的话
	// 挂载点里就是 @syno 的内容，既列不出 @syno 本身，也无法给它 set-default。
	tmp := "/mnt/_adopt_tmp"
	if dryRun {
		if commander.IsMountpoint(tmp) {
			step("清理残留临时挂载", "umount "+tmp, "pending",
				"检测到上次运行残留的临时挂载，演练：将先清理再挂载")
		}
		step("临时挂载 FS tree", "mount -o subvolid=5 "+dm+" "+tmp, "pending", "演练")
		step("设置默认子卷 @syno", "btrfs subvolume set-default "+strconv.Itoa(p.SynoSubvolID)+" "+tmp, "pending", "演练")
		step("卸载临时挂载", "umount "+tmp, "pending", "演练")
	} else {
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return fmt.Errorf("创建临时挂载点失败: %w", err)
		}
		// 幂等：上次运行中断可能残留了临时挂载，同目录重复 mount 会直接失败，
		// 先按"先查再跳过 + busy 惰性降级"的统一语义清掉
		if commander.IsMountpoint(tmp) {
			if err := umountTolerant("清理残留临时挂载", tmp, false, rec); err != nil {
				return fmt.Errorf("残留临时挂载 %s 清理失败: %w", tmp, err)
			}
		}
		// 挂 FS tree 而非默认子卷，否则列不出 @syno 本身
		if err := runner("临时挂载 FS tree", "mount", "-o", "subvolid=5", dm, tmp); err != nil {
			// 这一步失败时 mount 只会吐一句 "can't read superblock"，
			// 真正的原因（root flag / inode flag / 真损坏）只在 dmesg 里，
			// 所以必须把内核日志一起带出来，否则用户只能看到 exit status 32。
			return errors.New(explainFSTreeMountFailure(dm, err.Error()))
		}
		// 执行期重新定位 @syno（防计划与执行间状态变化）
		// 注意读的是**挂载点** tmp，不是设备 dm：v6.2 对块设备会报 not a directory
		synoID := 0
		for _, sv := range commander.BtrfsSubvolIDs(tmp) {
			if sv.Path == "@syno" {
				synoID = sv.ID
				break
			}
		}
		if synoID == 0 {
			_, _ = commander.Run(30*time.Second, "umount", tmp)
			return fmt.Errorf("临时挂载后未找到 @syno 子卷，已中止（临时挂载点已卸载）")
		}
		if err := runner("设置默认子卷 @syno", "btrfs", "subvolume", "set-default",
			strconv.Itoa(synoID), tmp); err != nil {
			_, _ = commander.Run(30*time.Second, "umount", tmp)
			return err
		}
		if err := umountTolerant("卸载临时挂载", tmp, false, rec); err != nil {
			return err
		}
	}

	// 8. 正式挂载（trimacl 失败自动降级；转正模式下已挂载则跳过）
	if dryRun {
		step("挂载数据卷 "+p.MountPoint,
			"mount -o rw,noatime,trimacl,space_cache=v2 "+dm+" "+p.MountPoint, "pending", "演练")
	} else {
		// 幂等：转正模式 / 重复执行时设备可能已挂在目标挂载点
		already := false
		for _, mp := range commander.MountPointsOf(dm) {
			if mp == p.MountPoint {
				already = true
				break
			}
		}
		if already {
			// 已挂载也要检查挂载参数：缺 trimacl 的卷在飞牛里加不了应用授权，
			// 而"已挂载，跳过"会把它当成成功，用户后面只会看到莫名的 Request failed。
			if commander.MountHasOption(p.MountPoint, "trimacl") {
				step("挂载数据卷 "+p.MountPoint, "(设备已挂载在目标挂载点)", "ok", "跳过")
			} else if err := ensureTrimacl(p.MountPoint); err != nil {
				step("挂载数据卷 "+p.MountPoint, "(设备已挂载，但缺 trimacl)", "ok",
					"⚠ 该挂载点已存在但**没有 trimacl 选项**，且现场补加失败："+err.Error()+
						"。文件读写不受影响，但飞牛「应用 → 访问权限」给它加目录会报 "+
						"Request failed（影视/相册加不进来）。请重启后由系统按默认参数重挂，"+
						"或手工执行: modprobe trimafs2; mount -o remount,trimacl "+p.MountPoint)
			} else {
				step("挂载数据卷 "+p.MountPoint, "(设备已挂载，已补加 trimacl)", "ok",
					"原挂载缺 trimacl，已 remount 补上")
			}
		} else {
			if err := os.MkdirAll(p.MountPoint, 0o755); err != nil {
				return fmt.Errorf("创建 %s 失败: %w", p.MountPoint, err)
			}
			sl := rec.Add("挂载数据卷 "+p.MountPoint,
				"mount -o rw,noatime,trimacl,space_cache=v2 "+dm+" "+p.MountPoint)

			// trimacl 是飞牛 trimafs2 模块提供的挂载选项，也是飞牛「应用 → 访问权限」
			// （影视/相册加目录）能生效的前提。模块没加载时带 trimacl 挂必然失败，
			// 以前直接"降级挂上去"就算成功——卷能用、文件能读，但飞牛的授权接口
			// 对该卷一律返回 "Request failed, please try again later"，
			// 用户完全看不出根因（真机 2026-10-31 /vol2 /vol4 就是这样）。
			// 所以先把模块点起来，失败再重试一次，实在不行才降级并**显著告警**。
			_, _ = commander.Run(60*time.Second, "modprobe", "trimafs2")

			const wantOpts = "rw,noatime,trimacl,space_cache=v2"
			const fallbackOpts = "rw,noatime,space_cache=v2"
			r, err := commander.Run(60*time.Second, "mount", "-o", wantOpts, dm, p.MountPoint)
			if err != nil {
				// 模块刚加载可能还没就绪，或上面那次 modprobe 失败：再探一次后原样重试
				_, _ = commander.Run(60*time.Second, "modprobe", "trimafs2")
				r, err = commander.Run(60*time.Second, "mount", "-o", wantOpts, dm, p.MountPoint)
			}
			if err != nil {
				sl.Done("pending", "trimacl 仍不可用，降级挂载: "+summarize(r.Stderr, ""))
				r2, err2 := commander.Run(60*time.Second, "mount", "-o", fallbackOpts, dm, p.MountPoint)
				if err2 != nil {
					sl.Done("fail", summarize(r2.Stderr, err2.Error()))
					return err2
				}
				sl.Done("ok", "⚠ 已降级挂载（无 trimacl）。该卷的文件能正常读写，但飞牛"+
					"「应用 → 访问权限」给它加目录会报 Request failed（影视/相册加不进来）。"+
					"排查：确认 trimafs2 模块已加载；或重启后由系统按默认参数重新挂载")
			} else {
				sl.Done("ok", summarize(r.Stdout, ""))
			}
		}
	}

	// 9. mount 表备份 + 注册
	// pg_dump / psql 一律走 RunPostgres：优先 sudo -u postgres，sudo 缺失时降级 runuser；
	// 失败时在日志里给出可直接复制的手工命令。
	dumpArgs := []string{"-d", "trim", "-t", "mount", "--data-only", "--column-inserts"}
	dumpManual := commander.ManualPostgres("pg_dump", dumpArgs...)
	if dryRun {
		step("备份 mount 表", dumpManual, "pending", "演练")
		step("注册 mount 表", "INSERT INTO mount (id="+strconv.Itoa(p.VolID)+", uuid="+p.MUUID+", ...) VALUES (...) ON CONFLICT DO NOTHING", "pending", "演练")
	} else {
		r, err := commander.RunPostgres(60*time.Second, "pg_dump", dumpArgs...)
		sl := rec.Add("备份 mount 表", dumpManual)
		if err != nil {
			sl.Done("fail", summarize(r.Stderr, err.Error())+"\n可手工执行: "+dumpManual)
			return fmt.Errorf("mount 表备份失败，拒绝注册: %w", err)
		}
		if wErr := os.WriteFile(filepath.Join(backupDir, "mount_before.sql"), []byte(r.Stdout), 0o600); wErr != nil {
			sl.Done("fail", "备份写入失败: "+wErr.Error())
			return wErr
		}
		sl.Done("ok", fmt.Sprintf("已写入 %s（%d 字节）", filepath.Join(backupDir, "mount_before.sql"), len(r.Stdout)))

		// 注册前先查一遍：把冲突显式报告给用户，而不是交给 SQL 静默吞掉
		if rows, qErr := MountTable(); qErr == nil {
			for _, row := range rows {
				if row.ID == p.VolID && row.UUID != p.MUUID {
					return fmt.Errorf("编号冲突：mount 表 id=%d 已存在且 uuid=%s（与本次 %s 不同），"+
						"请先确认该存储空间的来源，或手工清理该行后再接管", p.VolID, row.UUID, p.MUUID)
				}
				if row.UUID == p.MUUID && row.ID != p.VolID {
					return fmt.Errorf("uuid 冲突：mount 表已存在 uuid=%s（id=%d），拒绝重复注册", row.UUID, row.ID)
				}
			}
		}

		// 转正模式：mount 表已有同 uuid 行（上方冲突检查已确认 id/uuid 与计划一致），
		// 跳过 INSERT，仅记录日志
		if p.Converge {
			step("注册 mount 表", "(转正模式：复用已有注册行 id="+strconv.Itoa(p.VolID)+")", "ok", "跳过 INSERT")
		} else {
			// ON CONFLICT 一律不带目标列：mount 表的 uuid 未必建了唯一约束，
			// 写 ON CONFLICT (uuid) 在缺约束时整条 SQL 直接报错，会让接管在最后一步失败。
			sql := fmt.Sprintf(
				"INSERT INTO mount (id,uuid,comment,sync_speed_min,sync_speed_max,stripe_cache_size,warn_frsize,grow,state,pool_type) "+
					"VALUES (%d,'%s','',-1,-1,4096,10,'',0,0) ON CONFLICT DO NOTHING",
				p.VolID, p.MUUID)
			psqlArgs := []string{"-d", "trim", "-P", "pager=off", "-c", sql}
			psqlManual := commander.ManualPostgres("psql", psqlArgs...)
			if _, err := commander.RunPostgres(60*time.Second, "psql", psqlArgs...); err != nil {
				step("注册 mount 表", psqlManual, "fail",
					summarize("", err.Error())+"\n可手工执行: "+psqlManual)
				return fmt.Errorf("注册 mount 表失败（磁盘已挂载到 %s，可手工补注册或到「已接管卷」回滚）: %w",
					p.MountPoint, err)
			}
			step("注册 mount 表", psqlManual, "ok", "已写入")
		}
	}

	// 10. 验证
	if dryRun {
		step("验证", "(演练模式跳过验证)", "ok", "演练")
	} else {
		mounted := false
		for _, mp := range ProcMounts() {
			if mp == p.MountPoint {
				mounted = true
				break
			}
		}
		sl := rec.Add("验证挂载点", "检查 /proc/self/mounts 含 "+p.MountPoint)
		if !mounted {
			sl.Done("fail", p.MountPoint+" 未出现在系统挂载表")
			return fmt.Errorf("验证失败: %s 未挂载", p.MountPoint)
		}
		sl.Done("ok", p.MountPoint+" 挂载生效")

		// 写测试（教程 touch .wtest 等价实现，Go 原生读写自己创建的文件）
		slW := rec.Add("验证可写", "写读删 "+p.MountPoint+"/.fn-syno-adopt-wtest")
		wtest := filepath.Join(p.MountPoint, ".fn-syno-adopt-wtest")
		if err := os.WriteFile(wtest, []byte("adopt-write-test"), 0o644); err != nil {
			slW.Done("fail", "写入失败（卷可能被以只读挂载）: "+err.Error())
			return fmt.Errorf("验证失败: %s 不可写", p.MountPoint)
		}
		if b, err := os.ReadFile(wtest); err != nil || string(b) != "adopt-write-test" {
			_ = os.Remove(wtest)
			slW.Done("fail", "读回校验失败")
			return fmt.Errorf("验证失败: 写入数据读回不一致")
		}
		_ = os.Remove(wtest)
		slW.Done("ok", "写入/读回/清理均正常")

		sl2 := rec.Add("验证 mount 表", "SELECT id,uuid FROM mount WHERE id="+strconv.Itoa(p.VolID))
		rows, err := MountTable()
		found := false
		if err == nil {
			for _, r := range rows {
				if r.ID == p.VolID && r.UUID == p.MUUID {
					found = true
					break
				}
			}
		}
		if !found {
			sl2.Done("fail", "mount 表未查到 id="+strconv.Itoa(p.VolID)+" uuid="+p.MUUID)
			return fmt.Errorf("验证失败: mount 表注册未生效")
		}
		sl2.Done("ok", fmt.Sprintf("id=%d uuid=%s 已注册", p.VolID, p.MUUID))

		// 10.5 mdadm 阵列持久化：没有这步，重启后 1.2 超级块阵列不会自动组装，
		//     /volN 挂载不上。单盘 Basic 一样受益（群晖数据分区也走在 md 上）。
		if p.Kind == PlanKindDirect {
			PersistMdadmConfDevs(p.MdUUIDs, []string{p.MdDev}, rec)
		} else {
			PersistMdadmConf(p.VGUuid, rec)
		}

		// 11. 写接管记录：这是回滚的唯一依据（飞牛原生卷同样叫 trim_xxx/0，
		// 没有记录就无法区分"我们接管的"和"飞牛自己的"）
		recKind := p.Kind
		if recKind == "" {
			recKind = PlanKindLVM
		}
		mdUUIDs := p.MdUUIDs
		if recKind == PlanKindLVM {
			mdUUIDs = mdUUIDsOfVG(p.VGUuid)
		}
		if err := SaveRecord(Record{
			Kind:   recKind,
			VGUuid: p.VGUuid, OldVG: p.OldVG, NewVG: p.NewVG, OldLV: p.OldLV,
			VolID: p.VolID, MountPoint: p.MountPoint, MUUID: p.MUUID,
			Disk: p.Disk, LVSize: p.LVSize, Time: Stamp(),
			MdUUIDs: mdUUIDs, MdDev: p.MdDev,
		}); err != nil {
			step("记录接管信息", "", "warn",
				"写入 "+recordsPath()+" 失败: "+err.Error()+
					"（不影响已接管的卷，但「已接管卷 / 回滚」里不会出现它）")
		} else {
			step("记录接管信息", "", "ok", "已写入 "+recordsPath()+"（需要时可在「已接管卷」中回滚）")
		}

		// 12. 收尾指引：飞牛 UI 的刷新时机与后续动作
		step("完成", "", "ok", fmt.Sprintf(
			"接管成功：%s 已挂载并注册为存储空间 %d。"+
				"若飞牛存储管理器未立即显示，请刷新页面或重启系统；"+
				"接下来建议到「清理工具」修复文件所有权并清理 @eaDir。", p.MountPoint, p.VolID))
	}
	return nil
}

// shortUUID 安全截断 UUID 用于展示（防越界 panic）。
func shortUUID(u string) string {
	if len(u) > 12 {
		return u[:12]
	}
	return u
}

// fileHasStr 判断文件内容是否包含指定字符串（用于核对备份是否真的覆盖了目标卷组）。
// 文件读不到一律返回 false——核对不出证据就当作没备份，宁可误报也不放过。
func fileHasStr(path, needle string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte(needle))
}

// summarize 截断命令输出做日志摘要。
func summarize(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = fallback
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	if s == "" {
		return "（无输出）"
	}
	return s
}
