// Package eadir 实现群晖遗留 @eaDir 缩略图/索引目录的清理（v0.4）。
//
// 安全设计（应用绝不直接删数据）：
//   - rm 永不进入命令白名单
//   - 清理 = 把 @eaDir 移入卷根的回收站目录
//     /volN/.fn-syno-adopt-trash/<时间戳>/<父目录>_@eaDir
//   - 最终删除由用户在飞牛文件管理器中手动清空回收站完成，
//     期间任何误判都可以原样移回
package eadir

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
	"fnos-syno-adopt/owner"
)

// TrashDirName 卷根下的回收站目录名。
const TrashDirName = ".fn-syno-adopt-trash"

// maxSample 返回给前端的样例上限。
const maxSample = 200

// ScanResult @eaDir 扫描结果。
type ScanResult struct {
	Path       string   `json:"path"`
	Count      int      `json:"count"`
	Bytes      int64    `json:"bytes"`
	BytesHuman string   `json:"bytesHuman"`
	Sample     []string `json:"sample"`
	Truncated  bool     `json:"truncated"`
}

// trashPruneArgs 生成 @eaDir 查找参数：
// 必须先把回收站目录 prune 掉，否则第二次清理会把上次已搬进回收站的
// @eaDir（回收站内保留原目录树，路径同样以 /@eaDir 结尾）再搬一遍。
func trashPruneArgs(path string) []string {
	return []string{
		path, "-xdev",
		"-name", TrashDirName, "-prune", "-o",
		"-type", "d", "-name", "@eaDir", "-prune", "-print",
	}
}

// isEaDirLine 判定 find 输出行是否为待清理的 @eaDir（双保险：排除回收站内路径）。
func isEaDirLine(line string) bool {
	if !strings.HasSuffix(line, "/@eaDir") {
		return false
	}
	return !strings.Contains(line, "/"+TrashDirName+"/")
}

// Scan 查找路径下全部 @eaDir 并统计占用空间。
func Scan(path string) (*ScanResult, error) {
	path, err := owner.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	var dirs []string
	r, err := commander.RunStream(30*time.Minute, "find", func(line string) {
		line = strings.TrimSpace(line)
		if isEaDirLine(line) {
			dirs = append(dirs, line)
		}
	}, trashPruneArgs(path)...)
	if err != nil && r.ExitCode != 0 {
		return nil, fmt.Errorf("find 扫描失败: %s", r.Stderr)
	}

	res := &ScanResult{
		Path:       path,
		Count:      len(dirs),
		Sample:     []string{},
		BytesHuman: "0",
	}
	for i, d := range dirs {
		if i >= maxSample {
			res.Truncated = true
			break
		}
		res.Sample = append(res.Sample, d)
	}
	res.Bytes = duBytes(dirs)
	res.BytesHuman = human(res.Bytes)
	return res, nil
}

// duBytes 分批 du 统计字节量（避免单次参数列表超过 ARG_MAX）。
func duBytes(dirs []string) int64 {
	const batch = 400
	var total int64
	for i := 0; i < len(dirs); i += batch {
		end := i + batch
		if end > len(dirs) {
			end = len(dirs)
		}
		args := append([]string{"-s", "-B1", "--"}, dirs[i:end]...)
		r, err := commander.Run(20*time.Minute, "du", args...)
		if err != nil {
			continue // 统计失败不影响主流程
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			f := strings.Fields(line)
			if len(f) == 2 {
				if n, e := strconv.ParseInt(f[0], 10, 64); e == nil {
					total += n
				}
			}
		}
	}
	return total
}

// human 字节数人性化。
func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// volRootOf 取路径的卷根：/vol3/photo/x -> /vol3。
var volRootRE = regexp.MustCompile(`^/vol\d+`)

// Clean 把路径下全部 @eaDir 移入回收站。dryRun=true 只演练。
func Clean(path string, dryRun bool, rec *jobs.Recorder) error {
	step := func(name, command, status, detail string) {
		rec.Add(name, command).Done(status, detail)
	}
	path, err := owner.ValidatePath(path)
	if err != nil {
		return err
	}
	volRoot := volRootRE.FindString(path)
	if volRoot == "" {
		return fmt.Errorf("无法确定卷根目录: %s", path)
	}

	// 重新扫描（不用 UI 传列表，防篡改）
	var dirs []string
	r, err := commander.RunStream(30*time.Minute, "find", func(line string) {
		line = strings.TrimSpace(line)
		if isEaDirLine(line) {
			dirs = append(dirs, line)
		}
	}, trashPruneArgs(path)...)
	if err != nil && r.ExitCode != 0 {
		return fmt.Errorf("find 扫描失败: %s", r.Stderr)
	}
	if len(dirs) == 0 {
		step("扫描 @eaDir", "find "+path+" -name '@eaDir'", "ok", "未发现任何 @eaDir，无需处理")
		return nil
	}

	ts := time.Now().Format("20060102_150405")
	trash := filepath.Join(volRoot, TrashDirName, ts)

	if dryRun {
		step("建立回收站目录", "mkdir -p "+trash, "pending", "演练")
		step(fmt.Sprintf("移动 %d 个 @eaDir 入回收站", len(dirs)),
			"mv <@eaDir...> "+trash+"/（共 "+strconv.Itoa(len(dirs))+" 个）", "pending",
			"演练模式，未移动任何文件。回收站位置: "+trash)
		return nil
	}

	if err := os.MkdirAll(trash, 0o700); err != nil {
		return fmt.Errorf("创建回收站目录失败: %w", err)
	}
	step("建立回收站目录", "mkdir -p "+trash, "ok", trash)

	sl := rec.Add(fmt.Sprintf("移动 @eaDir 入回收站（0/%d）", len(dirs)),
		"mv <@eaDir...> "+trash+"/")
	moved, failed := 0, 0
	var failSample []string
	for _, d := range dirs {
		// 目标：回收站内保留原目录树 trash/<相对路径>/@eaDir。
		// 不扁平化：扁平命名会把 photo/sub 与 photo_sub 压成同一个名字，
		// mv 到已存在的目录时会"移进去"变成 photo_sub_@eaDir/@eaDir，
		// 破坏"可原样移回"的承诺。保留目录树后移回只需一条 mv。
		rel := strings.TrimPrefix(d, volRoot)    // /photo/sub/@eaDir
		rel = strings.TrimSuffix(rel, "/@eaDir") // /photo/sub
		rel = strings.Trim(rel, "/")             // photo/sub
		if strings.Contains(rel, "..") {
			failed++
			if len(failSample) < 5 {
				failSample = append(failSample, d+": 路径非法（含 ..），已跳过")
			}
			continue
		}
		parent := filepath.Join(trash, filepath.FromSlash(rel)) // rel 为空时 = trash 根
		dst := filepath.Join(parent, "@eaDir")
		if mkErr := os.MkdirAll(parent, 0o700); mkErr != nil {
			failed++
			if len(failSample) < 5 {
				failSample = append(failSample, d+": 建目录失败 "+mkErr.Error())
			}
			continue
		}
		if _, sErr := os.Lstat(dst); sErr == nil { // 目标已存在：绝不覆盖，直接跳过
			failed++
			if len(failSample) < 5 {
				failSample = append(failSample, d+": 目标已存在 "+dst+"，已跳过")
			}
			continue
		}
		r2, err := commander.Run(2*time.Minute, "mv", d, dst)
		if err != nil {
			failed++
			if len(failSample) < 5 {
				failSample = append(failSample, d+": "+firstLine(r2.Stderr))
			}
			continue
		}
		moved++
		if moved%500 == 0 {
			sl.Done("pending", fmt.Sprintf("进度 %d/%d（失败 %d）", moved, len(dirs), failed))
		}
	}
	if failed == 0 {
		sl.Done("ok", fmt.Sprintf("已移动 %d 个 @eaDir 到 %s。请在飞牛文件管理器中检查并清空该目录完成最终删除", moved, trash))
	} else {
		// 部分成功不算整体失败，但明确报告
		sl.Done("ok", fmt.Sprintf("已移动 %d 个、失败 %d 个。失败样例: %s",
			moved, failed, strings.Join(failSample, "；")))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
