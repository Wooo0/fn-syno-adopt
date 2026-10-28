// mdadm 阵列持久化：把接管的阵列写进 mdadm.conf，保证重启后能自动组装。
//
// 为什么必须做：群晖阵列用的是 1.2 超级块，内核不会自动组装。
// 接管成功当下能挂载，靠的是本次会话里已经 --assemble 过；
// 重启后没有 mdadm.conf 里的 ARRAY 记录（也没有 initramfs 里的元数据），
// 阵列可能不出现 → /volN 挂载失败 → 用户以为数据没了。
// 单盘 Basic 同样是 mdadm 1.2（系统分区/数据分区都走在 md 上），一样吃这个红利。
//
// 设计：只追加带 "# fn-syno-adopt" 标记的行，其它行一个字节都不动；
// 回滚时按 UUID 精确移除自己写过的行。
package adopt

import (
	"fmt"
	"os"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
)

// confMark 写入行的标记（回滚时靠它识别"这是我写的"）。
const confMark = "# fn-syno-adopt"

// mdConfPaths 候选路径：Debian/fnOS 用 /etc/mdadm/mdadm.conf，部分发行版用 /etc/mdadm.conf。
var mdConfPaths = []string{"/etc/mdadm/mdadm.conf", "/etc/mdadm.conf"}

// mdConfPath 返回要写入的配置文件：优先已存在的，都不存在则用 Debian 标准路径。
func mdConfPath() string {
	for _, p := range mdConfPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return mdConfPaths[0]
}

// mdUUIDsOfVG 返回该卷组底层 md 阵列的 UUID 列表（无 md 则返回空）。
func mdUUIDsOfVG(vgUuid string) []string {
	var out []string
	for _, pv := range commander.PVsOfVG(vgUuid) {
		if !strings.HasPrefix(pv, "/dev/md") {
			continue
		}
		if d := commander.MdDetailOf(pv); d.UUID != "" {
			out = append(out, d.UUID)
		}
	}
	return out
}

// PersistMdadmConf 把卷底层 md 阵列的 ARRAY 行追加进 mdadm.conf。
//
// 安全约束：
//   - 只读现有文件 + 追加，绝不覆盖用户原有配置
//   - 同一 UUID 已存在（不论谁写的）就不重复追加
//   - 写入失败不视为接管失败（数据已挂载注册成功），只记 warn 并给出手工指引
func PersistMdadmConf(vgUuid string, rec *jobs.Recorder) {
	step := func(name, command, status, detail string) {
		rec.Add(name, command).Done(status, detail)
	}

	uuids := mdUUIDsOfVG(vgUuid)
	if len(uuids) == 0 {
		step("阵列开机自动组装", "(该卷底层不是 md 阵列)", "ok", "跳过")
		return
	}

	conf := mdConfPath()
	var lines []string
	for _, pv := range commander.PVsOfVG(vgUuid) {
		if !strings.HasPrefix(pv, "/dev/md") {
			continue
		}
		if l := commander.MdConfLine(pv); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		step("阵列开机自动组装", "mdadm --detail --scan <md>", "warn",
			"未能生成 ARRAY 行，跳过。建议手工确认: mdadm --detail --scan 后把 ARRAY 行追加进 "+conf)
		return
	}

	existing, _ := os.ReadFile(conf) // 读不到就当空文件处理
	body := string(existing)

	var add []string
	for i, l := range lines {
		if i < len(uuids) && uuids[i] != "" && strings.Contains(body, uuids[i]) {
			continue // 已存在，不重复写
		}
		add = append(add, l)
	}
	if len(add) == 0 {
		step("阵列开机自动组装", "检查 "+conf, "ok", "ARRAY 行已存在，无需重复写入")
		return
	}

	var buf strings.Builder
	if len(body) > 0 && !strings.HasSuffix(body, "\n") {
		buf.WriteString("\n")
	}
	buf.WriteString(fmt.Sprintf("%s %s\n", confMark, time.Now().Format("2006-01-02 15:04:05")))
	for _, l := range add {
		buf.WriteString(l + "\n")
	}

	f, err := os.OpenFile(conf, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		step("阵列开机自动组装", "追加 "+conf, "warn",
			"⚠ 写入失败: "+err.Error()+"。重启后阵列可能不会自动组装，请手工把下面几行追加进 "+conf+"：\n"+
				strings.Join(add, "\n"))
		return
	}
	if _, err := f.WriteString(buf.String()); err != nil {
		_ = f.Close()
		step("阵列开机自动组装", "追加 "+conf, "warn", "⚠ 写入失败: "+err.Error())
		return
	}
	_ = f.Close()
	step("阵列开机自动组装", "追加 "+conf, "ok",
		fmt.Sprintf("已写入 %d 行（带 %s 标记），重启后阵列会自动组装；回滚时会自动移除", len(add), confMark))
}

// RemoveMdadmConf 从 mdadm.conf 移除本应用写过的行（按 UUID 精确匹配）。
// 只动带 confMark 标记、且 UUID 在 uuids 里的行，其它内容原样保留。
func RemoveMdadmConf(uuids []string, rec *jobs.Recorder) {
	step := func(name, command, status, detail string) {
		rec.Add(name, command).Done(status, detail)
	}
	if len(uuids) == 0 {
		step("移除阵列开机自动组装", "(该卷没有记录 md 阵列)", "ok", "跳过")
		return
	}
	conf := mdConfPath()
	b, err := os.ReadFile(conf)
	if err != nil {
		if os.IsNotExist(err) {
			step("移除阵列开机自动组装", conf, "ok", "文件不存在，跳过")
			return
		}
		step("移除阵列开机自动组装", conf, "warn", "读取失败: "+err.Error())
		return
	}

	kept, removed := filterConfLines(string(b), uuids)
	if removed == 0 {
		step("移除阵列开机自动组装", conf, "ok", "未找到本应用写入的 ARRAY 行，文件未改动")
		return
	}
	if err := os.WriteFile(conf, []byte(kept), 0o644); err != nil {
		step("移除阵列开机自动组装", conf, "warn", "写回失败（文件未改动）: "+err.Error())
		return
	}
	step("移除阵列开机自动组装", conf, "ok", fmt.Sprintf("已移除 %d 行，其余内容未改动", removed))
}

// filterConfLines 按 UUID 移除本应用写过的行，返回新内容与移除行数。
// 规则：只删除"紧邻 # fn-syno-adopt 标记行之后、且 UUID 匹配"的 ARRAY 行；
// 标记行本身一并删除；其它任何行（含别人的 ARRAY、注释、空行）原样保留。
// 抽成纯函数以便单测——"不动用户原有配置"是本模块的核心承诺。
func filterConfLines(body string, uuids []string) (string, int) {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	removed := 0
	pendingMark := false
	markLine := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, confMark) {
			pendingMark, markLine = true, line // 标记行先扣住，看下一行是不是我们要删的 ARRAY
			continue
		}
		if pendingMark {
			pendingMark = false
			if matchUUID(trimmed, uuids) {
				removed++
				continue // 删除我们自己写的 ARRAY 行
			}
			// UUID 不匹配（别人的 ARRAY）：标记行与 ARRAY 行都原样保留
			kept = append(kept, markLine, line)
			continue
		}
		kept = append(kept, line)
	}
	if pendingMark && markLine != "" {
		kept = append(kept, markLine) // 文件末尾孤立的标记行，保留
	}
	return strings.Join(kept, "\n"), removed
}

// matchUUID 判断 ARRAY 行是否含有给定 UUID 之一。
func matchUUID(line string, uuids []string) bool {
	for _, u := range uuids {
		if u != "" && strings.Contains(line, u) {
			return true
		}
	}
	return false
}
