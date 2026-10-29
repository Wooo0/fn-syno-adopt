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
	"crypto/rand"
	"encoding/hex"
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

// Plan 一次接管的完整计划。
type Plan struct {
	VGUuid       string   `json:"vgUuid"`
	OldVG        string   `json:"oldVG"`
	OldLV        string   `json:"oldLV"` // 群晖原始逻辑卷名：volume_7 / lv / volume_1（真机上五花八门，不可写死）
	NewVG        string   `json:"newVG"`
	DMPath       string   `json:"dmPath"`
	VolID        int      `json:"volId"`
	MountPoint   string   `json:"mountPoint"`
	MUUID        string   `json:"muuid"` // mount 表 uuid = trim_<uuid>-0
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

	// @syno 子卷定位
	synoID := 0
	allSubs, subErr := commander.BtrfsSubvolIDsErr(c.DMPath)
	for _, sv := range allSubs {
		if sv.Path == "@syno" {
			synoID = sv.ID
			break
		}
	}
	if synoID == 0 {
		// 报错里带上实际找到的子卷，方便真机排查（比干巴巴一句"找不到"有用得多）
		names := []string{}
		for _, sv := range allSubs {
			names = append(names, sv.Path)
		}
		extra := ""
		if subErr != nil {
			extra = "（读取子卷失败: " + subErr.Error() + "）"
		} else if len(names) > 0 {
			extra = "（该卷上的子卷: " + strings.Join(names, "、") + "）"
		} else {
			extra = "（该卷上没有任何子卷，卷可能未激活——可先在接管页点『激活卷组』）"
		}
		return nil, fmt.Errorf("未找到 @syno 数据子卷，接管终止%s", extra)
	}
	p.SynoSubvolID = synoID

	// 编号分配
	rows, err := MountTable()
	if err != nil {
		p.Warnings = append(p.Warnings, "mount 表查询失败（"+err.Error()+"），编号仅按挂载点分配，执行时会再次校验")
	}
	p.VolID, p.Warnings = NextVolID(ProcMounts(), rows)
	if p.VolID == 0 {
		return nil, fmt.Errorf("无法分配存储空间编号: %s", strings.Join(p.Warnings, "；"))
	}
	p.MountPoint = fmt.Sprintf("/vol%d", p.VolID)

	// 冲突预检
	for _, mp := range ProcMounts() {
		if mp == p.MountPoint {
			return nil, fmt.Errorf("%s 已被占用", p.MountPoint)
		}
	}
	if _, err := os.Stat(p.MountPoint); err == nil {
		p.Warnings = append(p.Warnings, p.MountPoint+" 目录已存在（未挂载），执行时将复用")
	}

	p.Steps = []string{
		"modprobe crc32c（确保 btrfs 校验模块可用）",
		"vgcfgbackup 备份卷组配置（按 UUID 定位，文件名带 UUID 短串防覆盖） -> " + BackupRoot + "/<时间戳>/",
		"vgrename " + shortUUID(p.VGUuid) + "... -> " + p.NewVG + "（按 UUID 定位，重名免疫）",
		"lvrename " + p.NewVG + " " + p.OldLV + " 0（原始卷名，非写死的 volume_1）",
		"vgchange -ay " + p.NewVG,
		"mount " + p.DMPath + " /mnt/_adopt_tmp（临时挂载设默认子卷）",
		"btrfs subvolume set-default " + strconv.Itoa(p.SynoSubvolID) + " /mnt/_adopt_tmp（@syno）",
		"umount /mnt/_adopt_tmp",
		"mkdir " + p.MountPoint,
		"mount -o rw,noatime,trimacl,space_cache=v2 " + p.DMPath + " " + p.MountPoint,
		"pg_dump 备份 mount 表 -> " + BackupRoot + "/<时间戳>/mount_before.sql",
		"INSERT INTO mount (id=" + strconv.Itoa(p.VolID) + ", uuid=" + p.MUUID + ")",
		"mdadm.conf 持久化：追加 ARRAY 行（带 # fn-syno-adopt 标记），保证重启后阵列自动组装",
		"验证：挂载点生效 + mount 表含新记录",
	}
	return p, nil
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
	if !dryRun {
		if _, err := os.Stat(p.MountPoint); err == nil {
			return fmt.Errorf("预检失败: %s 已存在", p.MountPoint)
		}
		for _, mp := range ProcMounts() {
			if mp == p.MountPoint {
				return fmt.Errorf("预检失败: %s 已被挂载", p.MountPoint)
			}
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

	// 3. vgcfgbackup
	//
	// 两个坑（多块群晖盘的卷组全都叫 vg1）：
	//   a. 按名字定位在多 VG 重名时行为不可预期 —— 一律按 UUID 定位
	//   b. 备份文件名若只用 vg1.bak，同秒接管多块盘会互相覆盖 —— 文件名带 UUID 短串
	//
	// 备份失败降级为警告而非中断：vgrename/lvrename 本身可逆，且后面还有
	// pg_dump 兜底；但在日志里必须醒目标注，让用户知道这一步没有备份。
	vgcfg := filepath.Join(backupDir, p.OldVG+"-"+shortUUID(p.VGUuid)+".bak")
	vgcfgArgs := []string{"-f", vgcfg, p.VGUuid}
	if dryRun {
		step("备份卷组配置", "vgcfgbackup "+strings.Join(vgcfgArgs, " "), "pending", "演练")
	} else if err := runner("备份卷组配置", "vgcfgbackup", vgcfgArgs...); err != nil {
		step("备份卷组配置（警告）", "vgcfgbackup "+strings.Join(vgcfgArgs, " "), "warn",
			"⚠ 卷组配置备份失败，本次接管没有 LVM 元数据备份！建议手工执行：vgcfgbackup -f "+
				vgcfg+" "+p.VGUuid+" ；流程继续（vgrename 可逆），如不放心中止后手工处理。")
	}

	// 4. vgrename（按 UUID 定位，多块 vg1 重名免疫）
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

	dm := "/dev/mapper/" + p.NewVG + "-0"

	// 7. 临时挂载设默认子卷
	tmp := "/mnt/_adopt_tmp"
	if dryRun {
		step("临时挂载", "mount "+dm+" "+tmp, "pending", "演练")
		step("设置默认子卷 @syno", "btrfs subvolume set-default "+strconv.Itoa(p.SynoSubvolID)+" "+tmp, "pending", "演练")
		step("卸载临时挂载", "umount "+tmp, "pending", "演练")
	} else {
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return fmt.Errorf("创建临时挂载点失败: %w", err)
		}
		if err := runner("临时挂载", "mount", dm, tmp); err != nil {
			return err
		}
		// 执行期重新定位 @syno（防计划与执行间状态变化）
		synoID := 0
		for _, sv := range commander.BtrfsSubvolIDs(dm) {
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
		if err := runner("卸载临时挂载", "umount", tmp); err != nil {
			return err
		}
	}

	// 8. 正式挂载（trimacl 失败自动降级）
	if dryRun {
		step("挂载数据卷 "+p.MountPoint,
			"mount -o rw,noatime,trimacl,space_cache=v2 "+dm+" "+p.MountPoint, "pending", "演练")
	} else {
		if err := os.MkdirAll(p.MountPoint, 0o755); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", p.MountPoint, err)
		}
		sl := rec.Add("挂载数据卷 "+p.MountPoint,
			"mount -o rw,noatime,trimacl,space_cache=v2 "+dm+" "+p.MountPoint)
		r, err := commander.Run(60*time.Second, "mount",
			"-o", "rw,noatime,trimacl,space_cache=v2", dm, p.MountPoint)
		if err != nil {
			sl.Done("pending", "trimacl 选项不受支持，降级重试: "+summarize(r.Stderr, ""))
			r2, err2 := commander.Run(60*time.Second, "mount",
				"-o", "rw,noatime,space_cache=v2", dm, p.MountPoint)
			if err2 != nil {
				sl.Done("fail", summarize(r2.Stderr, err2.Error()))
				return err2
			}
			sl.Done("ok", "已用降级选项挂载（无 trimacl）")
		} else {
			sl.Done("ok", summarize(r.Stdout, ""))
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
		PersistMdadmConf(p.VGUuid, rec)

		// 11. 写接管记录：这是回滚的唯一依据（飞牛原生卷同样叫 trim_xxx/0，
		// 没有记录就无法区分"我们接管的"和"飞牛自己的"）
		if err := SaveRecord(Record{
			VGUuid: p.VGUuid, OldVG: p.OldVG, NewVG: p.NewVG, OldLV: p.OldLV,
			VolID: p.VolID, MountPoint: p.MountPoint, MUUID: p.MUUID,
			Disk: p.Disk, LVSize: p.LVSize, Time: Stamp(),
			MdUUIDs: mdUUIDsOfVG(p.VGUuid),
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
