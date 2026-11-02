package adopt

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
)

// 半接管（孤儿）盘：本工具已经把卷组名改成 trim_<32hex>、逻辑卷名改成 0，
// 但接管在中途失败——没挂载、没注册、也没写进 adopted.json。
//
// 这类盘在界面里会彻底隐身，是真机 2026-10-31 暴露的严重可用性缺口：
//   - 检测面板 / 磁盘接管只认群晖原样的 vgN/volume_N，改名后不再匹配
//   - 「已接管卷」列表纯读 adopted.json，没有记录就不显示
//
// 于是用户既看不到它、也无从回滚，只能对着一个"消失的盘"干瞪眼。
//
// 判定条件必须保守——飞牛自己的存储空间同样叫 trim_<32hex>/0，误判会让人
// 把系统盘回滚掉。因此四道锁同时满足才认定为孤儿：
//  1. 卷组名精确匹配 trim_<32 位小写 hex>（本工具的命名规则）
//  2. 逻辑卷名恰好是 0（本工具改的名）
//  3. 该 UUID 不在 adopted.json（有记录的是正常接管，走原列表）
//  4. 当前没有挂载、且 mount 表里也没有该 uuid（飞牛存储空间必然挂着且已注册）
var orphanVGRE = regexp.MustCompile(`^trim_[0-9a-f]{32}$`)

// legacyBadVG 是历史 bug 留下的错误卷名：v0.8.5 解析 LVM 归档时把区块名
// `physical_volumes` 当成了卷组名，回滚时真的把卷组改名成了它。
// 这里额外放行这一种名字，让受害者能在界面上把它一键改回去
// （原始名仍由 LVM 归档回溯，那条链路已修好并有格式校验）。
const legacyBadVG = "physical_volumes"

// ListOrphans 扫描半接管盘，返回可直接回滚的视图（与「已接管卷」同结构）。
func ListOrphans() ([]AdoptedView, error) {
	lvs, err := commander.LVs()
	if err != nil {
		return nil, err
	}
	recs, _ := LoadRecords()
	known := map[string]bool{}
	for _, r := range recs {
		known[r.VGUuid] = true
	}
	rows, rowsErr := MountTable()
	registered := map[string]bool{}
	if rowsErr == nil {
		for _, row := range rows {
			registered[row.UUID] = true
		}
	}

	out := []AdoptedView{}
	for _, lv := range lvs {
		okName := orphanVGRE.MatchString(lv.VG) && lv.LV == "0"
		// 兼容历史 bug 产物（见 legacyBadVG 注释）
		if !okName && !(lv.VG == legacyBadVG && commander.IsSynoLVName(lv.LV)) {
			continue
		}
		if known[lv.VGUuid] {
			continue
		}
		// dm 名 = 卷组名-逻辑卷名。正常孤儿是 <trim_xxx>-0；历史 bug 留下的
		// physical_volumes 盘则已经是 physical_volumes-lv，不能再写死 "-0"。
		muuid := lv.VG + "-" + lv.LV
		if registered[muuid] {
			continue // mount 表有条目 = 正常接管过的存储空间，别碰
		}
		dm := lv.DMPath
		if dm == "" {
			dm = "/dev/mapper/" + muuid
		}
		if _, serr := os.Stat(dm); serr != nil {
			continue
		}
		mps := commander.MountPointsOf(dm)
		if len(mps) > 0 {
			continue // 已挂载的一律不当孤儿处理（宁可漏报，不误伤）
		}

		v := AdoptedView{
			VGUuid: lv.VGUuid, VG: lv.VG, LV: lv.LV,
			DMPath: dm, LVSize: lv.Size, LVActive: true,
			Orphan: true, Time: "",
		}
		v.Disk = commander.DiskOfVG(lv.VG)

		o, ok := commander.OriginOf(lv.VGUuid)
		intro := fmt.Sprintf(
			"⚠ 接管中途失败：卷组已改名为 %s、逻辑卷已改名为 0，但没挂载、没注册、也没写入接管记录"+
				"——所以它既不出现在检测面板，也不在下面的正常列表里。", lv.VG)
		if lv.VG == legacyBadVG {
			intro = "⚠ 卷组名被上一版程序的解析错误改成了 " + legacyBadVG + "（区块名被误当成卷组名），" +
				"数据与逻辑卷都没问题，只是卷组名不规范、检测面板认不出。"
		}
		if ok {
			v.OldVG = o.VG
			if len(o.LVs) > 0 {
				v.OldLV = o.LVs[0]
			}
			v.Rollbackable = v.OldLV != ""
			v.Notes = append(v.Notes, fmt.Sprintf(
				"%s原名 %s/%s 由 LVM 归档回溯得到，可一键还原。",
				intro, o.VG, v.OldLV))
		} else {
			v.Rollbackable = false
			v.Notes = append(v.Notes, fmt.Sprintf(
				"%s且没能从 /etc/lvm/archive 里回溯出 %s 的原卷组/逻辑卷名，"+
					"无法自动回滚。需手工确认后执行：lvrename %s %s <原卷名>；vgrename %s <原卷组名>",
				intro, lv.VG, lv.VG, lv.LV, lv.VG))
		}
		out = append(out, v)
	}
	return out, nil
}

// RollbackOrphan 回滚一块没有接管记录的半接管盘：把卷组名和逻辑卷名改回
// LVM 归档里记录的原样。不改 mount 表（本来就没注册）、不动 mdadm.conf。
func RollbackOrphan(vgUuid string, dryRun bool, rec *jobs.Recorder) error {
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

	lvs, err := commander.LVs()
	if err != nil {
		return fmt.Errorf("读取 LVM 失败: %w", err)
	}
	var curVG, curLV string
	found := false
	for _, lv := range lvs {
		if lv.VGUuid == vgUuid {
			curVG, curLV, found = lv.VG, lv.LV, true
			break
		}
	}
	if !found {
		return fmt.Errorf("系统中找不到该卷组（UUID=%s），可能盘已拔出", vgUuid)
	}
	step("定位卷组", "lvs -o vg_name,vg_uuid（按 UUID 定位）", "ok",
		fmt.Sprintf("当前 %s/%s（无接管记录，按半接管盘回滚）", curVG, curLV))

	o, ok := commander.OriginOf(vgUuid)
	if !ok || len(o.LVs) == 0 {
		return fmt.Errorf("无法从 %s 回溯原始卷名，请手工回滚：lvrename %s %s <原卷名>；vgrename %s <原卷组名>",
			"/etc/lvm/archive", curVG, curLV, curVG)
	}
	origLV := o.LVs[0]
	step("回溯原始卷名", "读 "+o.From, "ok", "原始 "+o.VG+"/"+origLV)

	dm := "/dev/mapper/" + curVG + "-" + curLV
	if err := unmountAllFor(dm, dryRun, rec); err != nil {
		return err
	}

	// 原名重名检查：整卡直通下多块群晖盘都叫 vg1，改回去之前必须确认没被占。
	vgs, err := commander.VGs()
	if err != nil {
		return fmt.Errorf("读取卷组列表失败: %w", err)
	}
	for _, g := range vgs {
		if g.VG == o.VG && g.VGUuid != vgUuid {
			pvs := commander.PVsOfVG(g.VGUuid)
			return fmt.Errorf(
				"原名 %s 已被其它卷组占用（UUID=%s，物理盘 %s）。请先回滚占用它的那块盘，"+
					"或手工执行：vgrename %s <临时名>",
				o.VG, g.VGUuid, strings.Join(pvs, ","), o.VG)
		}
	}
	step("检查重名冲突", "vgs -o vg_name,vg_uuid", "ok", "原名 "+o.VG+" 未被占用")

	if dryRun {
		step("回滚逻辑卷名", "lvrename "+curVG+" "+curLV+" "+origLV, "pending", "演练")
		step("回滚卷组名", "vgrename "+vgUuid+" "+o.VG, "pending", "演练")
		return nil
	}

	if curLV != origLV {
		if err := runner("回滚逻辑卷名", "lvrename", curVG, curLV, origLV); err != nil {
			return fmt.Errorf("回滚逻辑卷名失败（可手工: lvrename %s %s %s）: %w", curVG, curLV, origLV, err)
		}
	} else {
		step("回滚逻辑卷名", "(已是 "+origLV+")", "ok", "跳过")
	}
	if err := runner("回滚卷组名", "vgrename", vgUuid, o.VG); err != nil {
		return fmt.Errorf("回滚卷组名失败（逻辑卷已还原，可手工: vgrename %s %s）: %w", vgUuid, o.VG, err)
	}
	step("完成", "", "ok", fmt.Sprintf(
		"已还原为 %s/%s，回到「检测面板」刷新即可重新看到这块盘。", o.VG, origLV))
	return nil
}
