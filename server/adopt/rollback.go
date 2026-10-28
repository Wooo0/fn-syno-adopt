// 回滚：把本应用接管过的卷还原成群晖原样（vg1/volume_1），并撤掉飞牛注册。
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
	VGUuid       string   `json:"vgUuid"`
	VG           string   `json:"vg"`    // 当前卷组名（trim_xxx）
	LV           string   `json:"lv"`    // 当前逻辑卷名（0）
	OldVG        string   `json:"oldVG"` // 原名 vg1
	DMPath       string   `json:"dmPath"`
	LVSize       string   `json:"lvSize"`
	Disk         string   `json:"disk"`
	VolID        int      `json:"volId"`
	MountPoint   string   `json:"mountPoint"` // 记录中的挂载点
	Mounted      bool     `json:"mounted"`    // 当前是否真的挂着
	Registered   bool     `json:"registered"` // mount 表是否还有该行
	LVActive     bool     `json:"lvActive"`
	Time         string   `json:"time"`
	Rollbackable bool     `json:"rollbackable"`
	VgNameBusy   bool     `json:"vgNameBusy"` // 原名 vg1 已被其它卷组占用
	BusyBy       string   `json:"busyBy"`     // 占用者提示（UUID/物理盘）
	Notes        []string `json:"notes"`
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
		if mp := commander.MountPointOf(v.DMPath); mp != "" {
			v.Mounted = true
			if mp != r.MountPoint {
				v.Notes = append(v.Notes, "当前挂载点 "+mp+" 与接管记录 "+r.MountPoint+" 不一致，回滚将卸载 "+mp)
				v.MountPoint = mp
			}
		} else {
			v.Notes = append(v.Notes, "当前未挂载")
		}
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

// Rollback 执行一次回滚（dryRun=true 只演练）。
// 步骤：卸载 → 校验 vg1 未被占用 → vgrename 回 vg1 → lvrename 回 volume_1
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
		return fmt.Errorf("接管记录中没有 UUID=%s 的卷（只能回滚本应用接管过的盘）", vgUuid)
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

	// 1. 卸载
	dm := "/dev/mapper/" + curVG + "-" + curLV
	if mp := commander.MountPointOf(dm); mp != "" {
		if dryRun {
			step("卸载 "+mp, "umount "+mp, "pending", "演练")
		} else if err := runner("卸载 "+mp, "umount", mp); err != nil {
			return fmt.Errorf("卸载 %s 失败（可能有进程占用，先停掉相关服务/文件管理器再试）: %w", mp, err)
		}
	} else {
		step("卸载", "(当前未挂载)", "ok", "跳过")
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
