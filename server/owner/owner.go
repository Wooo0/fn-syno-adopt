// Package owner 实现群晖盘数据的所有权修复（v0.3）。
//
// 背景：群晖用户 uid 与飞牛不一致（如群晖 admin=1026），接管的卷上文件
// 属主仍是群晖 uid，飞牛用户无法读写。修复 = 递归 chown 到飞牛用户
// + 可选删除群晖遗留的扩展 ACL（setfacl -Rb）。
//
// 安全设计：
//   - 路径必须匹配 ^/vol\d+(/.*)?$，禁止根路径与系统目录
//   - 正式执行必须显式 confirm=REPAIR
//   - 扫描为流式，支持千万级文件
package owner

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
)

// volPathRE 允许操作的路径：/volN 开头（N≥1），且不得是挂载点本身之外的系统目录。
var volPathRE = regexp.MustCompile(`^/vol\d+(/.*)?$`)

// ValidatePath 校验并规范化用户输入的存储路径。
func ValidatePath(p string) (string, error) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if !volPathRE.MatchString(p) {
		return "", fmt.Errorf("路径必须以 /volN 开头（如 /vol3 或 /vol3/photo），收到: %s", p)
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("路径不存在或不可访问: %s", p)
	}
	return p, nil
}

// OwnerStat 一组 uid:gid 的文件统计。
type OwnerStat struct {
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	Files int    `json:"files"`
	Dirs  int    `json:"dirs"`
	Kind  string `json:"kind"` // 群晖默认 / 疑似飞牛 / 其他
}

// ScanResult 所有权扫描结果。
type ScanResult struct {
	Path      string       `json:"path"`
	Total     int          `json:"total"`
	Stats     []OwnerStat  `json:"stats"`
	Truncated bool         `json:"truncated"`
	Suggest   *SuggestUser `json:"suggest"`
}

// SuggestUser 来自 /etc/passwd 的飞牛本地用户（uid>=1000）。
type SuggestUser struct {
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Name string `json:"name"`
}

// StatLine find 单行解析结果。
type StatLine struct {
	UID   int
	GID   int
	Files int
	Dirs  int
}

// Scan 流式扫描路径下全部文件的所有权分布（find -xdev -printf）。
func Scan(path string) (*ScanResult, error) {
	path, err := ValidatePath(path)
	if err != nil {
		return nil, err
	}
	counts := map[string]*StatLine{}
	total := 0
	r, err := commander.RunStream(30*time.Minute, "find", func(line string) {
		// 行格式: uid|gid|f|d
		f := strings.SplitN(line, "|", 3)
		if len(f) < 2 {
			return
		}
		uid, e1 := strconv.Atoi(f[0])
		gid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			return
		}
		isDir := len(f) == 3 && f[2] == "d"
		key := f[0] + ":" + f[1]
		st, ok := counts[key]
		if !ok {
			st = &StatLine{UID: uid, GID: gid}
			counts[key] = st
		}
		if isDir {
			st.Dirs++
		} else {
			st.Files++
		}
		total++
	}, path, "-xdev", "-printf", "%U|%G|%y\n")

	res := &ScanResult{Path: path, Total: total, Stats: []OwnerStat{}}
	if err != nil && r.ExitCode != 0 {
		return res, fmt.Errorf("find 扫描失败: %s", r.Stderr)
	}
	for _, st := range counts {
		res.Stats = append(res.Stats, OwnerStat{
			UID: st.UID, GID: st.GID, Files: st.Files, Dirs: st.Dirs, Kind: classify(st.UID),
		})
	}
	sort.Slice(res.Stats, func(i, j int) bool {
		return res.Stats[i].Files+res.Stats[i].Dirs > res.Stats[j].Files+res.Stats[j].Dirs
	})
	if len(res.Stats) > 50 {
		res.Stats = res.Stats[:50]
		res.Truncated = true
	}
	res.Suggest = Suggest()
	return res, nil
}

// classify 粗略归类 uid。
// 顺序不可调整：群晖默认用户段（1024~1100，admin=1026）是 uid>=1000 的子集，
// 必须先判 syno，否则群晖遗留 uid 会被误标为"本地用户段"，UI 上的"以此为目标"
// 按钮会对它可见，用户可能把整卷 chown 到一个群晖遗留 uid 上。
func classify(uid int) string {
	switch {
	case uid == 0:
		return "root"
	case uid >= 1024 && uid < 1100:
		return "syno" // 群晖默认用户段（admin=1026 起）
	case uid >= 1000 && uid < 65534:
		return "local"
	default:
		return "system"
	}
}

// Suggest 从 /etc/passwd 找出可用的飞牛本地用户（uid>=1000 且非 nobody）。
func Suggest() *SuggestUser {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil
	}
	var best *SuggestUser
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		uid, e1 := strconv.Atoi(f[2])
		gid, e2 := strconv.Atoi(f[3])
		if e1 != nil || e2 != nil || uid < 1000 || uid >= 65534 {
			continue
		}
		if f[0] == "nobody" {
			continue
		}
		if best == nil || uid < best.UID {
			best = &SuggestUser{UID: uid, GID: gid, Name: f[0]}
		}
	}
	return best
}

// Repair 执行所有权修复。dryRun=true 只演练。
// 步骤：chown -R uid:gid path（必选）；setfacl -R -b path（可选）。
func Repair(path string, uid, gid int, removeAcl, dryRun bool, rec *jobs.Recorder) error {
	step := func(name, command, status, detail string) {
		rec.Add(name, command).Done(status, detail)
	}
	path, err := ValidatePath(path)
	if err != nil {
		return err
	}
	owner := fmt.Sprintf("%d:%d", uid, gid)

	// 预检：目标用户必须真实存在，防止把整卷 chown 到不存在的 uid
	found := false
	if b, e := os.ReadFile("/etc/passwd"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Split(line, ":")
			if len(f) >= 3 && f[2] == strconv.Itoa(uid) {
				found = true
				break
			}
		}
	}
	if !found {
		return fmt.Errorf("uid=%d 在本机 /etc/passwd 中不存在，拒绝执行（防止整卷属主变成无效用户）", uid)
	}

	run := func(stepName, name string, args ...string) error {
		sl := rec.Add(stepName, name+" "+strings.Join(args, " "))
		r, err := commander.Run(4*time.Hour, name, args...)
		if err != nil {
			d := r.Stderr
			if d == "" {
				d = err.Error()
			}
			if len(d) > 300 {
				d = d[:300] + "..."
			}
			sl.Done("fail", d)
			return err
		}
		sl.Done("ok", "完成")
		return nil
	}

	if dryRun {
		step("变更属主（演练）", "chown -R "+owner+" "+path, "pending", "演练模式，未修改任何文件")
		if removeAcl {
			step("清除扩展 ACL（演练）", "setfacl -R -b "+path, "pending", "演练模式，未修改任何文件")
		}
		return nil
	}

	if err := run("变更属主", "chown", "-R", owner, path); err != nil {
		return err
	}
	if removeAcl {
		// setfacl 可能未安装：缺失时降级为跳过并提示
		if err := run("清除扩展 ACL", "setfacl", "-R", "-b", path); err != nil {
			step("清除扩展 ACL（降级说明）", "", "skip",
				"setfacl 执行失败（可能未安装）。群晖 ACL 若未被清除，部分共享权限可能继承旧规则，可手动执行: setfacl -R -b "+path)
			return nil // ACL 清理失败不视为整体失败（chown 已完成）
		}
	}
	return nil
}
