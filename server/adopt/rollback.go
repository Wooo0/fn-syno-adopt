// 回滚：把本应用接管过的卷还原成群晖原样（原卷组名/原逻辑卷名），并撤掉飞牛注册。
//
// 典型场景：接管中途失败（vgrename 已成功、mount 表注册失败），盘停在"
// 半接管"状态——VG 已叫 trim_xxx，而检测面板会跳过 trim_ 前缀的卷，
// 用户在 UI 里彻底看不到这块盘，只能 SSH 手救。
//
// 两个关键约束：
//  1. 只能回滚 Records 里有记录的卷。飞牛原生卷同样叫 trim_xxx/0，
//     误回滚用户自己的存储空间是灾难性的。
//  2. 改名回 vg1 前必须检测重名。整卡直通下多块群晖盘都叫 vg1，
//     若还有别的卷占着 vg1，LVM 会拒绝改名，必须先处理占用者。
package adopt

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
)

// AdoptedView 一条"本应用已接管"的卷（供 UI 展示与回滚）。
type AdoptedView struct {
	VGUuid       string `json:"vgUuid"`
	VG           string `json:"vg"`    // 当前卷组名（trim_xxx）
	LV           string `json:"lv"`    // 当前逻辑卷名（0）
	OldVG        string `json:"oldVG"` // 原名 vg1
	OldLV        string `json:"oldLV"` // 原逻辑卷名 volume_6 / lv（半接管盘靠 LVM 归档回溯）
	DMPath       string `json:"dmPath"`
	LVSize       string `json:"lvSize"`
	Disk         string `json:"disk"`
	VolID        int    `json:"volId"`
	MountPoint   string `json:"mountPoint"` // 记录中的挂载点
	Mounted      bool   `json:"mounted"`    // 当前是否真的挂着
	Registered   bool   `json:"registered"` // mount 表是否还有该行
	LVActive     bool   `json:"lvActive"`
	Time         string `json:"time"`
	Rollbackable bool   `json:"rollbackable"`
	VgNameBusy   bool   `json:"vgNameBusy"` // 原名 vg1 已被其它卷组占用
	BusyBy       string `json:"busyBy"`     // 占用者提示（UUID/物理盘）
	// Orphan=true：接管中途失败的半接管盘——卷已改名但没记录、没挂载、没注册，
	// 靠扫描 trim_* 卷组发现的，不是从 adopted.json 读出来的。
	Orphan bool     `json:"orphan"`
	Notes  []string `json:"notes"`
}

// applyMounts 依据设备当前挂载点列表更新"已接管卷"视图。
//
// btrfs 可同时挂多个挂载点：接管挂载（/volN）与历史救援脚本的残留挂载
// （真机实测 /mnt/t2 只读残留）并存。优先认接管记录里的挂载点；残留的
// 其它挂载点写进 notes，回滚时会一并卸载。
func applyMounts(v *AdoptedView, recorded string, mps []string) {
	if len(mps) == 0 {
		v.Notes = append(v.Notes, "当前未挂载")
		return
	}
	v.Mounted = true
	var extra []string
	for _, mp := range mps {
		if recorded != "" && mp == recorded {
			v.MountPoint = recorded
		} else {
			extra = append(extra, mp)
		}
	}
	if v.MountPoint == "" {
		v.MountPoint = mps[0]
		if recorded != "" {
			v.Notes = append(v.Notes,
				"当前挂载点 "+v.MountPoint+" 与接管记录 "+recorded+" 不一致，回滚将卸载 "+v.MountPoint)
		}
	}
	if len(extra) > 0 {
		v.Notes = append(v.Notes,
			"检测到额外挂载点（"+strings.Join(extra, "、")+"，多为历史救援脚本残留）：回滚时将一并卸载")
	}
}

// dedupStrings 去重保序（md 与 dm 两个来源的挂载点合并后可能重复）。
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ListAdopted 汇总接管记录与当前系统状态，返回可回滚列表。
func ListAdopted() ([]AdoptedView, error) {
	recs, err := LoadRecords()
	if err != nil {
		return nil, err
	}
	lvs, lvsErr := commander.LVs()
	vgs, _ := commander.VGs()
	rows, rowsErr := MountTable()

	out := []AdoptedView{}
	for _, r := range recs {
		// —— direct（直建 btrfs，无 LVM）：没有卷组可查，走独立展示分支 ——
		if r.Kind == PlanKindDirect {
			v := AdoptedView{
				VGUuid: r.VGUuid, VG: r.MdDev, LV: "(直建 btrfs)", OldVG: "—",
				DMPath: r.MdDev, LVSize: r.LVSize, Disk: r.Disk,
				VolID: r.VolID, MountPoint: r.MountPoint, Time: r.Time,
				Notes: []string{},
			}
			if st, err := os.Stat(r.MdDev); err == nil && st.Mode()&os.ModeDevice != 0 {
				v.LVActive = true
			} else {
				v.Notes = append(v.Notes,
					"当前找不到阵列设备 "+r.MdDev+"（阵列未组装或重启后设备号漂移）：请先到「检测面板」点『组装阵列』")
			}
			// 挂载点要查两处：md 阵列本体 + dm-linear 包装设备。
			// 正式挂载以 dm 名义进行（/proc/mounts 设备字段是 /dev/mapper/trim_*-0），
			// md2 与 dm 是两个不同的设备节点（major 9 vs 253），只查 /dev/mdN
			// 会查不到 → 面板误报"当前未挂载"（真机实测 2026-10-30 /vol2）。
			mps := commander.MountPointsOf(r.MdDev)
			if r.MUUID != "" {
				mps = append(mps, commander.MountPointsOf("/dev/mapper/"+r.MUUID)...)
			}
			mps = dedupStrings(mps)
			applyMounts(&v, r.MountPoint, mps)
			if rowsErr != nil {
				v.Notes = append(v.Notes, "mount 表读取失败: "+rowsErr.Error())
			} else {
				for _, row := range rows {
					if row.ID == r.VolID && row.UUID == r.MUUID {
						v.Registered = true
						break
					}
				}
				if !v.Registered {
					v.Notes = append(v.Notes, "mount 表已无该记录（上次接管可能就停在这一步）")
				}
			}
			// 直建布局没有 LVM 改名，不存在"原名被占用"问题，始终可回滚
			v.Rollbackable = true
			out = append(out, v)
			continue
		}

		v := AdoptedView{
			VGUuid: r.VGUuid, OldVG: r.OldVG, VG: r.NewVG, LV: "0",
			VolID: r.VolID, MountPoint: r.MountPoint, Disk: r.Disk,
			LVSize: r.LVSize, Time: r.Time,
			Notes: []string{},
		}
		found := false
		for _, lv := range lvs {
			if lv.VGUuid != r.VGUuid {
				continue
			}
			found = true
			v.VG, v.LV, v.DMPath, v.LVSize = lv.VG, lv.LV, lv.DMPath, lv.Size
			v.LVActive = strings.Contains(lv.Attr, "a")
			break
		}
		if !found {
			if lvsErr != nil {
				v.Notes = append(v.Notes, "读取 LVM 失败: "+lvsErr.Error())
			} else {
				v.Notes = append(v.Notes, "系统中已找不到该卷组（可能已被手工改名或移除），请检查后手工处理")
			}
			out = append(out, v)
			continue
		}
		applyMounts(&v, r.MountPoint, commander.MountPointsOf(v.DMPath))
		if rowsErr != nil {
			v.Notes = append(v.Notes, "mount 表读取失败: "+rowsErr.Error())
		} else {
			for _, row := range rows {
				if row.ID == r.VolID && row.UUID == r.MUUID {
					v.Registered = true
					break
				}
			}
			if !v.Registered {
				v.Notes = append(v.Notes, "mount 表已无该记录（上次接管可能就停在这一步）")
			}
		}
		// 原名 vg1 是否被别的卷组占着（排除自己）
		if r.OldVG != "" && r.OldVG != v.VG {
			for _, g := range vgs {
				if g.VG == r.OldVG && g.VGUuid != r.VGUuid {
					v.VgNameBusy = true
					v.BusyBy = "卷组 UUID=" + shortUUID(g.VGUuid)
					v.Notes = append(v.Notes,
						"原名 "+r.OldVG+" 已被其它卷组占用（"+v.BusyBy+"），LVM 拒绝重名；请先回滚/处理占用该名的那块盘")
					break
				}
			}
		}
		v.Rollbackable = !v.VgNameBusy
		out = append(out, v)
	}
	return out, nil
}

// unmountAllFor 卸载设备的全部挂载点（含历史救援脚本残留的只读挂载）。
// dryRun=true 只演练不执行。
//
// 真机坑：umount 报 target is busy（exit 32）很常见——SSH 会话的当前目录
// 停在卷里、飞牛文件管理器/索引服务开着卷，都会占用。此时降级 umount -l
// （惰性卸载）：挂载点立刻从挂载树摘除（/proc/mounts 即刻消失），内核等
// 占用引用释放后自动收尾。对回滚安全：direct 路径无需改名；LVM 路径的
// vgrename/lvrename 本就允许在挂载状态下执行。
func unmountAllFor(dev string, dryRun bool, rec *jobs.Recorder) error {
	mps := commander.MountPointsOf(dev)
	if len(mps) == 0 {
		rec.Add("卸载", "(当前未挂载)").Done("ok", "跳过")
		return nil
	}
	for _, mp := range mps {
		sl := rec.Add("卸载 "+mp, "umount "+mp)
		if dryRun {
			sl.Done("pending", "演练")
			continue
		}
		r, err := commander.Run(120*time.Second, "umount", mp)
		if err == nil {
			sl.Done("ok", summarize(r.Stdout, ""))
			continue
		}
		// busy 降级：惰性卸载
		sl.Done("pending", "常规卸载失败（"+summarize(r.Stderr, "")+"），降级 umount -l 惰性卸载")
		r2, err2 := commander.Run(120*time.Second, "umount", "-l", mp)
		if err2 != nil {
			sl.Done("fail", summarize(r2.Stderr, err2.Error())+
				"｜排查占用：fuser -vm "+mp+" 或 lsof "+mp+
				"（常见：SSH 会话当前目录在卷里、文件管理器开着卷）")
			return fmt.Errorf("卸载 %s 失败（常规与惰性均失败）: %w", mp, err2)
		}
		sl.Done("ok", "已惰性卸载（占用引用释放后内核自动收尾）")
	}
	return nil
}

// Rollback 执行一次回滚（dryRun=true 只演练）。
// 步骤：卸载 → 校验原卷组名未被占用 → vgrename 回原名 → lvrename 回原逻辑卷名
//
//	→ 删除 mount 表行（先 pg_dump 备份）→ 删除接管记录。
func Rollback(vgUuid string, dryRun bool, rec *jobs.Recorder) error {
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

	recs, err := LoadRecords()
	if err != nil {
		return err
	}
	var tgt *Record
	for i := range recs {
		if recs[i].VGUuid == vgUuid {
			tgt = &recs[i]
			break
		}
	}
	if tgt == nil {
		// 没有记录不等于没被我们改过名：接管在写入记录前失败时（例如设置默认
		// 子卷报错），卷已经叫 trim_xxx 了，但 adopted.json 里什么都没有。
		// 这种半接管盘由 ListOrphans 列出来，回滚走这条专线。
		if o, oerr := ListOrphans(); oerr == nil {
			for _, v := range o {
				if v.VGUuid == vgUuid {
					return RollbackOrphan(vgUuid, dryRun, rec)
				}
			}
		}
		return fmt.Errorf("接管记录中没有 UUID=%s 的卷（只能回滚本应用接管过的盘）", vgUuid)
	}
	// —— direct（直建 btrfs，无 LVM）：没有卷组名可还原，走独立回滚分支 ——
	if tgt.Kind == PlanKindDirect {
		return rollbackDirect(tgt, dryRun, rec)
	}

	// 当前卷组名以系统为准（用户可能手工改过）
	curVG, curLV, active := "", "", false
	lvs, err := commander.LVs()
	if err != nil {
		return fmt.Errorf("读取 LVM 失败: %w", err)
	}
	lvFound := false
	for _, lv := range lvs {
		if lv.VGUuid == vgUuid {
			curVG, curLV, active = lv.VG, lv.LV, strings.Contains(lv.Attr, "a")
			lvFound = true
			break
		}
	}
	if !lvFound {
		return fmt.Errorf("系统中找不到该卷组（UUID=%s），无法回滚", vgUuid)
	}
	step("定位卷组", "lvs -o vg_name,vg_uuid（按 UUID 定位）", "ok",
		fmt.Sprintf("当前 %s/%s（激活=%v）", curVG, curLV, active))

	// 1. 卸载（含历史残留挂载，可能不止一个）
	dm := "/dev/mapper/" + curVG + "-" + curLV
	if err := unmountAllFor(dm, dryRun, rec); err != nil {
		return err
	}

	// 2. 重名检测：原名是否被别的卷组占用（GLM 指出的坑：整卡直通下多块盘都叫 vg1）
	vgs, err := commander.VGs()
	if err != nil {
		return fmt.Errorf("读取卷组列表失败: %w", err)
	}
	if tgt.OldVG != "" && tgt.OldVG != curVG {
		for _, g := range vgs {
			if g.VG == tgt.OldVG && g.VGUuid != vgUuid {
				pvs := commander.PVsOfVG(g.VGUuid)
				return fmt.Errorf(
					"原名 %s 已被其它卷组占用（UUID=%s，物理盘 %s）。LVM 不允许重名，"+
						"请先在下方列表中回滚占用 %s 的那块盘，或手工执行：vgrename %s <临时名>",
					tgt.OldVG, g.VGUuid, strings.Join(pvs, ","), tgt.OldVG, shortUUID(g.VGUuid))
			}
		}
	}
	step("检查重名冲突", "vgs -o vg_name,vg_uuid", "ok", "原名 "+tgt.OldVG+" 未被占用")

	if dryRun {
		step("回滚卷组名", "vgrename "+vgUuid+" "+tgt.OldVG, "pending", "演练")
		step("回滚逻辑卷名", "lvrename "+tgt.OldVG+" "+curLV+" "+tgt.OldLV, "pending", "演练")
		step("删除 mount 表行", "DELETE FROM mount WHERE id="+strconv.Itoa(tgt.VolID)+
			" AND uuid='"+tgt.MUUID+"'", "pending", "演练")
		step("移除阵列开机自动组装配置", "从 mdadm.conf 移除本应用写入的 ARRAY 行", "pending", "演练")
		step("删除接管记录", "(演练不写文件)", "pending", "演练")
		return nil
	}

	// 3. mount 表备份 + 删除注册（先备份，再删）
	ts := time.Now().Format("20060102_150405")
	backupDir := BackupRoot + "/" + ts
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	dumpArgs := []string{"-d", "trim", "-t", "mount", "--data-only", "--column-inserts"}
	r, err := commander.RunPostgres(60*time.Second, "pg_dump", dumpArgs...)
	sl := rec.Add("备份 mount 表", commander.ManualPostgres("pg_dump", dumpArgs...))
	if err != nil {
		sl.Done("fail", summarize(r.Stderr, err.Error())+"\n可手工执行: "+
			commander.ManualPostgres("pg_dump", dumpArgs...))
		return fmt.Errorf("mount 表备份失败，拒绝删除注册: %w", err)
	}
	if wErr := os.WriteFile(backupDir+"/mount_before_rollback.sql", []byte(r.Stdout), 0o600); wErr != nil {
		sl.Done("fail", "备份写入失败: "+wErr.Error())
		return wErr
	}
	sl.Done("ok", "已写入 "+backupDir+"/mount_before_rollback.sql")

	delSQL := fmt.Sprintf("DELETE FROM mount WHERE id=%d AND uuid='%s'", tgt.VolID, tgt.MUUID)
	delArgs := []string{"-d", "trim", "-P", "pager=off", "-c", delSQL}
	if _, err := commander.RunPostgres(60*time.Second, "psql", delArgs...); err != nil {
		step("删除 mount 表行", commander.ManualPostgres("psql", delArgs...), "fail",
			"删除失败，可手工执行: "+commander.ManualPostgres("psql", delArgs...))
		return fmt.Errorf("删除 mount 表行失败: %w", err)
	}
	step("删除 mount 表行", commander.ManualPostgres("psql", delArgs...), "ok", "已撤销注册")

	// 4. 改名回群晖原样（按 UUID 定位）
	if err := runner("回滚卷组名", "vgrename", vgUuid, tgt.OldVG); err != nil {
		return fmt.Errorf("回滚卷组名失败（卷仍处于 %s，可重试或手工: vgrename %s %s）: %w",
			curVG, vgUuid, tgt.OldVG, err)
	}
	if curLV != tgt.OldLV {
		if err := runner("回滚逻辑卷名", "lvrename", tgt.OldVG, curLV, tgt.OldLV); err != nil {
			return fmt.Errorf("回滚逻辑卷名失败（卷组已还原为 %s，可手工: lvrename %s %s %s）: %w",
				tgt.OldVG, tgt.OldVG, curLV, tgt.OldLV, err)
		}
	} else {
		step("回滚逻辑卷名", "(已是 "+tgt.OldLV+")", "ok", "跳过")
	}

	// 4.5 移除 mdadm.conf 里本应用写过的 ARRAY 行（其它行不动）
	RemoveMdadmConf(tgt.MdUUIDs, rec)

	// 5. 收尾：删除记录 + 提示
	if err := RemoveRecord(vgUuid); err != nil {
		step("删除接管记录", "", "warn", "记录删除失败（不影响已回滚的卷）: "+err.Error())
	} else {
		step("删除接管记录", "", "ok", "已从回滚列表移除")
	}
	step("完成", "", "ok", fmt.Sprintf(
		"已还原为 %s/%s。回到「检测面板」刷新即可重新看到这块群晖盘；"+
			"若存储管理器仍显示旧条目，刷新页面或重启系统。", tgt.OldVG, tgt.OldLV))
	return nil
}

// rollbackDirect 直建 btrfs（无 LVM 层）的回滚：卸载 → 删 mount 表注册
// → 移除 mdadm.conf 里本应用写入的 ARRAY 行 → 删除接管记录。
//
// 没有 vgrename/lvrename 可还原——接管时本来就没改任何名字，盘一直是
// 群晖原样（btrfs 默认子卷在接管时切到 @syno，与群晖原值一致，无需回拨）。
func rollbackDirect(tgt *Record, dryRun bool, rec *jobs.Recorder) error {
	step := func(name, command, status, detail string) {
		rec.Add(name, command).Done(status, detail)
	}

	// 0. 定位阵列设备（重启后设备号可能漂移，找不到就明确告诉用户先组装）
	if st, err := os.Stat(tgt.MdDev); err != nil || st.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("找不到阵列设备 %s（阵列未组装或设备号漂移）：请先到「检测面板」点『组装阵列』后重试", tgt.MdDev)
	}
	step("定位阵列", "确认设备节点 "+tgt.MdDev, "ok", "设备存在")

	// dm 包装设备与开机 unit（记录里的 MUUID 即 dm 名；早期 /dev/mdN 旧记录无 dm）
	dmDev := "/dev/mapper/" + tgt.MUUID
	unit := dmUnitPath(tgt.MUUID)
	hasUnit := tgt.MUUID != tgt.MdDev // 旧版 /dev/md2 记录 MUUID==MdDev，无 unit

	if dryRun {
		if err := unmountAllFor(dmDev, true, rec); err != nil {
			return err
		}
		if err := unmountAllFor(tgt.MdDev, true, rec); err != nil {
			return err
		}
		if hasUnit {
			step("停用开机自动包装 unit", "systemctl disable --now fn-syno-adopt-dm-"+tgt.MUUID+".service", "pending", "演练")
			step("删除 unit 文件", "rm "+unit, "pending", "演练")
			step("移除 dm-linear 映射", "dmsetup remove "+tgt.MUUID, "pending", "演练")
		}
		step("删除 mount 表行", "DELETE FROM mount WHERE id="+strconv.Itoa(tgt.VolID)+
			" AND uuid='"+tgt.MUUID+"'", "pending", "演练")
		step("移除阵列开机自动组装配置", "从 mdadm.conf 移除本应用写入的 ARRAY 行", "pending", "演练")
		step("删除接管记录", "(演练不写文件)", "pending", "演练")
		return nil
	}

	// 1. 卸载（dm 包装设备 + 裸 md 设备都可能有挂载，含历史残留）
	if err := unmountAllFor(dmDev, false, rec); err != nil {
		return err
	}
	if err := unmountAllFor(tgt.MdDev, false, rec); err != nil {
		return err
	}

	// 1.5 撤销开机自动包装 unit 与 dm 映射（尽力而为，单项失败不阻断回滚）
	if hasUnit {
		if _, err := commander.Run(30*time.Second, "systemctl", "disable", "--now",
			"fn-syno-adopt-dm-"+tgt.MUUID+".service"); err != nil {
			step("停用开机自动包装 unit", "systemctl disable --now ...", "warn", "停用失败（可能本就没启用）: "+err.Error())
		} else {
			step("停用开机自动包装 unit", "systemctl disable --now ...", "ok", "已停用")
		}
		if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
			step("删除 unit 文件", "rm "+unit, "warn", "删除失败: "+err.Error())
		} else {
			step("删除 unit 文件", "rm "+unit, "ok", "已删除")
		}
		_, _ = commander.Run(30*time.Second, "systemctl", "daemon-reload")
		if _, err := commander.Run(30*time.Second, "dmsetup", "remove", tgt.MUUID); err != nil {
			step("移除 dm-linear 映射", "dmsetup remove "+tgt.MUUID, "warn",
				"移除失败（可能有残留引用，重启后自动消失，不影响数据）: "+err.Error())
		} else {
			step("移除 dm-linear 映射", "dmsetup remove "+tgt.MUUID, "ok", "已移除")
		}
	}

	// 2. mount 表备份 + 删除注册（先备份，再删）
	ts := time.Now().Format("20060102_150405")
	backupDir := BackupRoot + "/" + ts
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	dumpArgs := []string{"-d", "trim", "-t", "mount", "--data-only", "--column-inserts"}
	r, err := commander.RunPostgres(60*time.Second, "pg_dump", dumpArgs...)
	sl := rec.Add("备份 mount 表", commander.ManualPostgres("pg_dump", dumpArgs...))
	if err != nil {
		sl.Done("fail", summarize(r.Stderr, err.Error())+"\n可手工执行: "+
			commander.ManualPostgres("pg_dump", dumpArgs...))
		return fmt.Errorf("mount 表备份失败，拒绝删除注册: %w", err)
	}
	if wErr := os.WriteFile(backupDir+"/mount_before_rollback.sql", []byte(r.Stdout), 0o600); wErr != nil {
		sl.Done("fail", "备份写入失败: "+wErr.Error())
		return wErr
	}
	sl.Done("ok", "已写入 "+backupDir+"/mount_before_rollback.sql")

	delSQL := fmt.Sprintf("DELETE FROM mount WHERE id=%d AND uuid='%s'", tgt.VolID, tgt.MUUID)
	delArgs := []string{"-d", "trim", "-P", "pager=off", "-c", delSQL}
	if _, err := commander.RunPostgres(60*time.Second, "psql", delArgs...); err != nil {
		step("删除 mount 表行", commander.ManualPostgres("psql", delArgs...), "fail",
			"删除失败，可手工执行: "+commander.ManualPostgres("psql", delArgs...))
		return fmt.Errorf("删除 mount 表行失败: %w", err)
	}
	step("删除 mount 表行", commander.ManualPostgres("psql", delArgs...), "ok", "已撤销注册")

	// 3. 移除 mdadm.conf 里本应用写过的 ARRAY 行（其它行不动）
	RemoveMdadmConf(tgt.MdUUIDs, rec)

	// 4. 删除记录 + 提示
	if err := RemoveRecord(tgt.VGUuid); err != nil {
		step("删除接管记录", "", "warn", "记录删除失败（不影响已回滚的卷）: "+err.Error())
	} else {
		step("删除接管记录", "", "ok", "已从回滚列表移除")
	}
	step("完成", "", "ok", fmt.Sprintf(
		"已撤销接管（%s 未做任何改名，盘始终保持群晖原样）。回到「检测面板」刷新即可重新看到这块盘；"+
			"若存储管理器仍显示旧条目，刷新页面或重启系统。", tgt.MdDev))
	return nil
}
