// Package vols 提供"可选的存储空间列表"，供清理工具下拉选择。
//
// 为什么要这个包：让用户手输 /volN 路径太容易出错——拼错、输成不存在的目录、
// 或者一上来就把 /vol1（飞牛系统卷）当目标。改成下拉选择后，用户看到的
// 是"实际挂载着的卷 + 容量 + 是不是刚接管的群晖盘"，不可能选错。
package vols

import (
	"sort"
	"strconv"
	"strings"

	"fnos-syno-adopt/adopt"
	"fnos-syno-adopt/commander"
)

// Volume 一个可选择的存储空间。
type Volume struct {
	Path      string `json:"path"`   // /vol3
	Device    string `json:"device"` // /dev/mapper/trim_xxx-0
	VG        string `json:"vg"`     // trim_xxx（无法定位时为空）
	LV        string `json:"lv"`     // 0
	FS        string `json:"fs"`     // btrfs
	Total     int64  `json:"total"`  // 总容量（字节）
	Used      int64  `json:"used"`   // 已用（字节）
	Avail     int64  `json:"avail"`  // 可用（字节）
	TotalHu   string `json:"totalHuman"`
	UsedHu    string `json:"usedHuman"`
	Adopted   bool   `json:"adopted"`   // 是否本应用接管的群晖盘
	Disk      string `json:"disk"`      // 物理盘（来自接管记录）
	AdoptTime string `json:"adoptTime"` // 接管时间
	IsSystem  bool   `json:"isSystem"`  // 疑似飞牛系统卷（/vol1 且非接管盘）
}

// List 返回当前已挂载的存储空间列表，按编号升序。
// 数据源：/proc/self/mounts（挂载点）+ lvs（卷组/逻辑卷）+ df（容量）
//   - 接管记录（标记"这是刚接管的群晖盘"）。全程只读。
func List() ([]Volume, error) {
	mounts := commander.VolMounts()
	lvs, _ := commander.LVs()
	recs, err := adopt.LoadRecords()
	if err != nil {
		recs = nil // 记录读不到不影响列表，只是少了标记
	}
	// 接管记录按挂载点索引
	byPath := map[string]adopt.Record{}
	for _, r := range recs {
		if r.MountPoint != "" {
			byPath[r.MountPoint] = r
		}
	}

	out := make([]Volume, 0, len(mounts))
	for _, m := range mounts {
		v := Volume{Path: m.Point, Device: m.Device, FS: m.FSType}
		for _, lv := range lvs {
			if commander.SameDevice(lv.DMPath, m.Device) {
				v.VG, v.LV = lv.VG, lv.LV
				break
			}
		}
		if t, u, a, e := commander.DfUsage(m.Point); e == nil {
			v.Total, v.Used, v.Avail = t, u, a
			v.TotalHu, v.UsedHu = human(t), human(u)
		}
		if rec, ok := byPath[m.Point]; ok {
			v.Adopted, v.Disk, v.AdoptTime = true, rec.Disk, rec.Time
		} else {
			// 没有接管记录且是 /vol1：飞牛系统盘，提醒用户别选错
			v.IsSystem = m.Point == "/vol1"
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return volNum(out[i].Path) < volNum(out[j].Path) })
	return out, nil
}

// Recommend 在列表里挑一个推荐目标：优先本应用接管的卷，其次非系统卷，最后第一个。
func Recommend(list []Volume) string {
	for _, v := range list {
		if v.Adopted {
			return v.Path
		}
	}
	for _, v := range list {
		if !v.IsSystem {
			return v.Path
		}
	}
	if len(list) > 0 {
		return list[0].Path
	}
	return ""
}

func volNum(p string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(p, "/vol"))
	return n
}

// human 字节数人性化（本地实现，避免与其它包产生耦合）。
func human(b int64) string {
	const unit = 1024
	if b <= 0 {
		return "0"
	}
	if b < unit {
		return strconv.FormatInt(b, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(b)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}
