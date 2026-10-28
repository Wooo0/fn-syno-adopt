// 检测模块：识别群晖 Basic 数据盘（分区 -> mdadm -> LVM vg1/volume_1 -> btrfs）。
// 本文件全部操作只读，绝不写盘。接管（写）逻辑在后续版本单独实现。
package commander

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- 数据模型 ----------

// BlockDevice 对应 lsblk --json 输出的设备节点。
type BlockDevice struct {
	Name     string        `json:"name"`
	Size     string        `json:"size"`
	Type     string        `json:"type"` // disk / part / md / ...
	FSType   string        `json:"fstype"`
	Model    string        `json:"model"`
	Serial   string        `json:"serial"`
	Children []BlockDevice `json:"children"`
}

type lsblkOut struct {
	BlockDevices []BlockDevice `json:"blockdevices"`
}

// LVInfo 一条 LVM 逻辑卷信息。
type LVInfo struct {
	VG     string
	VGUuid string // 卷组 UUID：群晖每块盘的卷组都叫 vg1，仅能靠 UUID 区分
	LV     string
	Size   string
	Attr   string
	DMPath string // /dev/mapper/vg1-volume_1
}

// SynoCandidate 描述一块疑似群晖数据盘。
type SynoCandidate struct {
	Disk      string   `json:"disk"` // 物理盘，如 /dev/sdd
	Model     string   `json:"model"`
	Serial    string   `json:"serial"`
	VG        string   `json:"vg"`
	VGUuid    string   `json:"vgUuid"` // 接管计划以 UUID 定位卷组，避免重名歧义
	LV        string   `json:"lv"`
	DMPath    string   `json:"dmPath"`
	LVSize    string   `json:"lvSize"`
	LVActive  bool     `json:"lvActive"`
	FS        string   `json:"fs"`
	Subvols   []string `json:"subvols"`
	Adoptable bool     `json:"adoptable"` // 满足接管前置条件（vg1/volume_1 + btrfs + 单盘）
	Notes     []string `json:"notes"`
}

// ---------- 只读探测 ----------

// Lsblk 返回全部块设备树（只读，安全）。
func Lsblk() ([]BlockDevice, error) {
	r, err := Run(15*time.Second, "lsblk", "--paths", "--json",
		"-o", "NAME,SIZE,TYPE,FSTYPE,MODEL,SERIAL")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Stdout) == "" {
		return nil, fmt.Errorf("lsblk 无输出（命令不可用或环境异常）")
	}
	var out lsblkOut
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		return nil, fmt.Errorf("解析 lsblk 输出失败: %w", err)
	}
	return out.BlockDevices, nil
}

// LVs 返回 LVM 逻辑卷列表（含卷组 UUID）。
func LVs() ([]LVInfo, error) {
	r, err := Run(10*time.Second, "lvs", "--noheadings", "--separator", "|",
		"-o", "vg_name,vg_uuid,lv_name,lv_size,lv_attr,lv_dm_path")
	if err != nil {
		return nil, err
	}
	var lvs []LVInfo
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 6 {
			continue
		}
		lvs = append(lvs, LVInfo{
			VG:     strings.TrimSpace(parts[0]),
			VGUuid: strings.TrimSpace(parts[1]),
			LV:     strings.TrimSpace(parts[2]),
			Size:   strings.TrimSpace(parts[3]),
			Attr:   strings.TrimSpace(parts[4]),
			DMPath: strings.TrimSpace(parts[5]),
		})
	}
	return lvs, nil
}

// PVsOfVG 返回指定卷组 UUID 对应的物理卷（PV）路径列表。
// 用 UUID 而非名字过滤：多块群晖盘接入时卷组全部重名 vg1，名字无法区分。
func PVsOfVG(vgUuid string) []string {
	r, err := Run(10*time.Second, "pvs", "--noheadings", "--separator", "|", "-o", "pv_name,vg_uuid")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 && strings.TrimSpace(parts[1]) == vgUuid {
			out = append(out, strings.TrimSpace(parts[0]))
		}
	}
	return out
}

// VGInfo 一条卷组信息（名称 + UUID）。
type VGInfo struct {
	VG     string
	VGUuid string
}

// VGs 返回全部卷组名与 UUID（只读）。
// 回滚前用它检测 vg1 是否已被其它卷组占用（整卡直通下多块群晖盘都叫 vg1）。
func VGs() ([]VGInfo, error) {
	r, err := Run(10*time.Second, "vgs", "--noheadings", "--separator", "|", "-o", "vg_name,vg_uuid")
	if err != nil {
		return nil, err
	}
	var out []VGInfo
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		out = append(out, VGInfo{VG: strings.TrimSpace(parts[0]), VGUuid: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

// MountPointOf 返回设备当前的挂载点（未挂载返回空串）。
// /proc/mounts 里的设备名可能是 /dev/mapper/xxx 也可能是 /dev/dm-N，
// 因此同时比对原始路径与其符号链接解析结果。
func MountPointOf(dev string) string {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return ""
	}
	targets := map[string]bool{dev: true}
	if real, e := filepath.EvalSymlinks(dev); e == nil {
		targets[real] = true
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && targets[f[0]] {
			return strings.ReplaceAll(f[1], "\\040", " ")
		}
	}
	return ""
}

// MdMembers 返回 md 阵列的成员分区路径（mdadm --detail）。
func MdMembers(mdPath string) []string {
	r, err := Run(10*time.Second, "mdadm", "--detail", mdPath)
	if err != nil {
		return nil
	}
	var members []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "/dev/") && !strings.HasPrefix(f, "/dev/md") {
				members = append(members, f)
			}
		}
	}
	return members
}

// FSType 返回设备上的文件系统类型。
func FSType(dev string) string {
	r, err := Run(10*time.Second, "blkid", "-o", "value", "-s", "TYPE", dev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// BtrfsSubvol 返回一个子卷的 ID 与路径。
type BtrfsSubvol struct {
	ID   int
	Path string
}

// BtrfsSubvolIDs 返回 btrfs 文件系统的子卷列表（含 ID，供 set-default 使用）。
func BtrfsSubvolIDs(dev string) []BtrfsSubvol {
	r, err := Run(20*time.Second, "btrfs", "subvolume", "list", dev)
	if err != nil {
		return nil
	}
	var subs []BtrfsSubvol
	for _, line := range strings.Split(r.Stdout, "\n") {
		if i := strings.Index(line, " path "); i < 0 {
			continue
		} else {
			path := strings.TrimSpace(line[i+6:])
			id := 0
			if _, err := fmt.Sscanf(line, "ID %d", &id); err == nil && id > 0 {
				subs = append(subs, BtrfsSubvol{ID: id, Path: path})
			}
		}
	}
	return subs
}

// MdExamineScan 只读扫描所有设备上的 mdadm 超级块（--examine 不激活、不写盘）。
// 注意：结果包含系统盘自身的阵列（飞牛系统分区也是 mdadm 1.2），
// 必须经 MdUnassembled 与已激活阵列比对后才能提示用户。
func MdExamineScan() []string {
	r, err := Run(30*time.Second, "mdadm", "--examine", "--scan")
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ARRAY") {
			lines = append(lines, line)
		}
	}
	return lines
}

// ActiveMdUUIDs 收集当前已激活 md 阵列的 UUID：
// 从 /proc/mdstat 取设备名，再逐一 mdadm --detail 提取 UUID。
func ActiveMdUUIDs() map[string]bool {
	b, err := os.ReadFile("/proc/mdstat")
	if err != nil {
		return nil
	}
	active := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		// 形如 "md0 : active raid1 sdb2[0] sda2[1]"
		if !strings.Contains(line, " : ") {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(line, " :", 2)[0])
		if name == "" || !strings.HasPrefix(name, "md") {
			continue
		}
		r, err := Run(10*time.Second, "mdadm", "--detail", "/dev/"+name)
		if err != nil {
			continue
		}
		for _, dl := range strings.Split(r.Stdout, "\n") {
			t := strings.TrimSpace(dl)
			if strings.HasPrefix(t, "UUID :") {
				u := strings.TrimSpace(strings.TrimPrefix(t, "UUID :"))
				if u != "" {
					active[u] = true
				}
				break
			}
		}
	}
	return active
}

// MdUnassembled 返回检测到超级块但尚未激活的阵列行（排除系统盘自身阵列）。
func MdUnassembled() []string {
	active := ActiveMdUUIDs()
	var out []string
	for _, line := range MdExamineScan() {
		uuid := ""
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "UUID=") {
				uuid = strings.TrimPrefix(f, "UUID=")
				break
			}
		}
		if uuid != "" && active[uuid] {
			continue // 已激活（如飞牛系统盘阵列），不提示
		}
		out = append(out, line)
	}
	return out
}

// MdAssemble 组装全部可组装的 mdadm 阵列（幂等：已激活的阵列自动跳过）。
func MdAssemble() (string, error) {
	r, err := Run(120*time.Second, "mdadm", "--assemble", "--scan")
	out := strings.TrimSpace(r.Stdout + "\n" + r.Stderr)
	return out, err
}

// KernelVersion 返回内核版本（uname -r）。
func KernelVersion() string {
	r, err := Run(5*time.Second, "uname", "-r")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// ---------- 汇总识别 ----------

// DetectSynologyDisks 汇总块设备 / LVM / 文件系统信息，
// 识别疑似群晖 Basic 数据盘（vg1/volume_1 + btrfs）。
// 返回：候选盘列表、完整块设备树、错误。全程只读。
func DetectSynologyDisks() ([]SynoCandidate, []BlockDevice, error) {
	disks, err := Lsblk()
	if err != nil {
		return nil, nil, err
	}
	if disks == nil {
		disks = []BlockDevice{}
	}
	lvs, err := LVs()
	if err != nil {
		return nil, nil, err
	}

	// 物理盘信息索引（按 /dev/sdX 名）
	diskInfo := map[string]BlockDevice{}
	for _, d := range disks {
		if d.Type == "disk" {
			diskInfo[d.Name] = d
		}
	}

	// 初始化为空切片：JSON 序列化为 [] 而非 null（前端契约：列表字段永不为 null）
	candidates := []SynoCandidate{}
	for _, lv := range lvs {
		if strings.HasPrefix(lv.VG, "trim_") {
			continue // 飞牛原生卷，跳过
		}

		c := SynoCandidate{
			VG:       lv.VG,
			VGUuid:   lv.VGUuid,
			LV:       lv.LV,
			LVSize:   lv.Size,
			DMPath:   lv.DMPath,
			LVActive: strings.Contains(lv.Attr, "a"),
			Subvols:  []string{}, // 初始化为空切片，JSON 序列化为 [] 而非 null
			Notes:    []string{},
		}

		// 物理盘归属：PV -> (md 阵列) -> 成员分区 -> 物理盘（按 VG UUID 匹配，容忍重名）
		var physDisks []string
		for _, pv := range PVsOfVG(lv.VGUuid) {
			switch {
			case strings.HasPrefix(pv, "/dev/md"):
				for _, m := range MdMembers(pv) {
					physDisks = append(physDisks, stripPartition(m))
				}
			case strings.HasPrefix(pv, "/dev/"):
				physDisks = append(physDisks, stripPartition(pv))
			}
		}
		physDisks = dedup(physDisks)
		switch {
		case len(physDisks) == 1:
			c.Disk = physDisks[0]
			if d, ok := diskInfo[c.Disk]; ok {
				c.Model, c.Serial = d.Model, d.Serial
			}
		case len(physDisks) > 1:
			c.Notes = append(c.Notes, fmt.Sprintf(
				"该卷组关联多块物理盘(%s)：疑似跨盘卷（RAID/多盘 volume），接管流程仅支持单盘 Basic 卷，已拒绝接管",
				strings.Join(physDisks, ", ")))
		default:
			c.Notes = append(c.Notes, "未能定位物理盘（md 阵列可能未组装，或为未激活的 LVM）")
		}

		if c.DMPath != "" {
			c.FS = FSType(c.DMPath)
			if c.FS == "btrfs" && c.LVActive {
				for _, sv := range BtrfsSubvolIDs(c.DMPath) {
					c.Subvols = append(c.Subvols, sv.Path)
				}
			}
		}

		c.Adoptable = c.FS == "btrfs" && c.VG == "vg1" && c.LV == "volume_1" && len(physDisks) == 1
		if c.FS == "ext4" {
			c.Notes = append(c.Notes, "ext4 卷：可挂载读取，但“接管”流程仅支持 btrfs")
		}
		if !c.LVActive && c.FS == "" {
			c.Notes = append(c.Notes, "逻辑卷未激活，无法读取文件系统类型（接管时会自动激活）")
		}
		candidates = append(candidates, c)
	}
	return candidates, disks, nil
}

// stripPartition 去掉分区号得到物理盘名：/dev/sdd3 -> /dev/sdd；/dev/nvme0n1p3 -> /dev/nvme0n1。
func stripPartition(dev string) string {
	base := strings.TrimPrefix(dev, "/dev/")
	// nvme / mmcblk: "p<数字>" 后缀
	if (strings.HasPrefix(base, "nvme") || strings.HasPrefix(base, "mmcblk")) &&
		strings.LastIndex(base, "p") > 0 && allDigits(base[strings.LastIndex(base, "p")+1:]) {
		return dev[:len(dev)-len(base)+strings.LastIndex(base, "p")]
	}
	// sd/hd/vd: 纯数字后缀
	i := len(dev)
	for i > 0 && dev[i-1] >= '0' && dev[i-1] <= '9' {
		i--
	}
	if i < len(dev) && i > len("/dev/") {
		return dev[:i]
	}
	return dev
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
