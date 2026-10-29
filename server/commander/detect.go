// 检测模块：识别群晖 Basic 数据盘（分区 -> mdadm -> LVM vg1/volume_1 -> btrfs）。
// 本文件全部操作只读，绝不写盘。接管（写）逻辑在后续版本单独实现。
package commander

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	DevReady  bool     `json:"devReady"` // 设备节点是否确实可用（卷组已激活、节点是块设备）
	FS        string   `json:"fs"`
	Subvols   []string `json:"subvols"`
	Adoptable bool     `json:"adoptable"` // 满足接管前置条件（vg1/volume_1 + btrfs + 单盘）
	Notes     []string `json:"notes"`

	// —— 类型与阵列信息（分类由检测决定，不由用户选择）——
	Kind        string   `json:"kind"`        // basic / raid / unsupported
	MdDev       string   `json:"mdDev"`       // 底层 md 阵列设备（如 /dev/md2），非阵列时为空
	RaidLevel   string   `json:"raidLevel"`   // raid5 / raid1 ...
	RaidMembers int      `json:"raidMembers"` // 阵列成员总数
	RaidActive  int      `json:"raidActive"`  // 当前在线成员数
	RaidHealth  string   `json:"raidHealth"`  // clean / degraded / recovering ...
	RaidSize    string   `json:"raidSize"`
	PhysDisks   []string `json:"physDisks"` // 关联的物理盘列表
}

// Kind 候选盘类型常量。
const (
	KindBasic       = "basic"       // 群晖 Basic 单盘：单 PV 且落在单块物理盘上
	KindRAID        = "raid"        // 单个 md 阵列、成员 ≥2（RAID1/5/6/10）
	KindUnsupported = "unsupported" // 真跨盘卷 / 阵列不健康 / 其它
)

// ReservedLV 群晖的保留区逻辑卷名（每块盘上都有一份，固定 12M，不是数据卷）。
const ReservedLV = "syno_vg_reserved_area"

var (
	vgNameRE  = regexp.MustCompile(`^vg\d+$`)
	volNameRE = regexp.MustCompile(`^volume_\d+$`)
)

// IsSynoDataVolume 判定是否为群晖数据卷（真机实测，不能只认 vg1/volume_1）。
//
// 群晖的命名有两种，混在同一台机器上是常态：
//   - 老式（DSM 6 及升级上来的）：vg1/volume_7、vg2/volume_6、vg3/volume_8
//   - DSM 7 新式：vg1001/lv、vg1002/lv
//
// 早期版本写成 `VG=="vg1" && LV=="volume_1"`，真机上 8 块盘全部落空。
func IsSynoDataVolume(vg, lv string) bool {
	if !vgNameRE.MatchString(vg) {
		return false
	}
	return lv == "lv" || volNameRE.MatchString(lv)
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
		// LVM 用 "<" 前缀表示该值是 extent 取整后的上限（如 <14.55t），展示时去掉更干净
		lvs = append(lvs, LVInfo{
			VG:     strings.TrimSpace(parts[0]),
			VGUuid: strings.TrimSpace(parts[1]),
			LV:     strings.TrimSpace(parts[2]),
			Size:   strings.TrimPrefix(strings.TrimSpace(parts[3]), "<"),
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

// VGActivate 按 UUID 激活卷组（幂等；已激活的执行也是安全的）。
// 必须按 UUID 而不是名字：多块群晖盘接进来时卷组全都叫 vg1，名字会歧义。
func VGActivate(vgUuid string) error {
	_, err := Run(60*time.Second, "vgchange", "-ay", vgUuid)
	return err
}

// isBlockDevMode 判断是否为块设备（排除字符设备：/dev/null 也带 ModeDevice）。
func isBlockDevMode(m os.FileMode) bool {
	return m&os.ModeDevice != 0 && m&os.ModeCharDevice == 0
}

// ResolveBlockDev 把一个 LVM 设备路径解析成**当前确实可用**的块设备路径。
//
// 真机坑：lvs 给出的 lv_dm_path 是 /dev/mapper/<vg>-<lv>，但卷组未激活时
// 这个节点要么不存在、要么是悬空符号链接，btrfs 直接报
// "not a directory / can't access"。这里依次尝试：
//  1. 原路径（/dev/mapper/x）
//  2. 符号链接解析后的真实节点（/dev/dm-N）
//  3. /dev/<vg>/<lv>
//
// 返回一个 stat 确认是块设备的路径；全都不可用时返回原因。
func ResolveBlockDev(dev, vg, lv string) (string, string) {
	candidates := []string{dev}
	if real, err := filepath.EvalSymlinks(dev); err == nil && real != dev {
		candidates = append(candidates, real)
	}
	if vg != "" && lv != "" {
		candidates = append(candidates, "/dev/"+vg+"/"+lv)
	}
	seen := map[string]bool{}
	for _, p := range candidates {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.Mode()&os.ModeDevice != 0 && st.Mode()&os.ModeCharDevice == 0 {
			return p, ""
		}
	}
	return "", fmt.Sprintf("%s 不是可用块设备（卷组未激活或设备节点缺失）", dev)
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

// VolMount 一个已挂载的存储空间挂载点（/volN）。
type VolMount struct {
	Point  string // /vol3
	Device string // /dev/mapper/trim_xxx-0
	FSType string // btrfs
}

// VolMounts 返回当前系统里所有已挂载的 /volN 存储空间（只读）。
// 清理工具的路径选择只应该在这些里面挑——没挂载的目录根本扫不了。
func VolMounts() []VolMount {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	var out []VolMount
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		mp := strings.ReplaceAll(f[1], "\\040", " ")
		if !isVolPoint(mp) {
			continue
		}
		out = append(out, VolMount{Point: mp, Device: f[0], FSType: f[2]})
	}
	return out
}

// isVolPoint 判断是否为 /volN 形式的挂载点（N 为 1-2 位数字，且不是 /volN/子路径）。
func isVolPoint(mp string) bool {
	if len(mp) < 5 || !strings.HasPrefix(mp, "/vol") {
		return false
	}
	for _, c := range mp[4:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// DfUsage 查询一个路径的容量（字节）。用 POSIX 格式解析，避免换行拆分。
func DfUsage(path string) (total, used, avail int64, err error) {
	r, err := Run(15*time.Second, "df", "-B1", "-P", path)
	if err != nil {
		return 0, 0, 0, err
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if len(lines) < 2 {
		return 0, 0, 0, fmt.Errorf("df 输出无法解析: %q", r.Stdout)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, 0, 0, fmt.Errorf("df 输出字段不足: %q", lines[len(lines)-1])
	}
	total, _ = strconv.ParseInt(f[1], 10, 64)
	used, _ = strconv.ParseInt(f[2], 10, 64)
	avail, _ = strconv.ParseInt(f[3], 10, 64)
	return total, used, avail, nil
}

// SameDevice 判断两个设备路径是否指向同一个设备（/dev/mapper/x 与 /dev/dm-N 视为相同）。
func SameDevice(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
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

// BtrfsSubvolIDsErr 返回 btrfs 文件系统的子卷列表（含 ID，供 set-default 使用），
// 并一并返回失败原因——真机上"读不到子卷"是高频问题，必须能把原因暴露给用户。
//
// 真机实测的失败有三类，必须先自检再下手，否则只丢一句 exit status 1 给用户：
//  1. 逻辑卷未激活 → 设备节点不存在（ERROR: not a directory / can't access）
//  2. 系统没装 btrfs-progs → 找不到 btrfs 命令
//  3. btrfs-progs 版本太旧 → 群晖 DSM 7.2 默认开 block-group-tree，旧版读不了
func BtrfsSubvolIDsErr(dev string) ([]BtrfsSubvol, error) {
	if dev == "" {
		return nil, fmt.Errorf("逻辑卷没有设备节点（lv_dm_path 为空）：卷组可能未激活")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		return nil, fmt.Errorf("系统缺少 btrfs-progs（找不到 btrfs 命令）：%v", err)
	}
	if st, err := os.Stat(dev); err != nil {
		return nil, fmt.Errorf("%s 不可访问（%v）：逻辑卷未激活，设备节点缺失", dev, err)
	} else if !isBlockDevMode(st.Mode()) {
		return nil, fmt.Errorf("%s 不是块设备（%s）：设备节点异常，请激活卷组后重试", dev, st.Mode().Type())
	}
	// 大卷（十几 T）上 subvolume list 可能较慢，超时给足
	r, err := Run(90*time.Second, "btrfs", "subvolume", "list", dev)
	if err != nil {
		return nil, fmt.Errorf("btrfs subvolume list %s 失败（exit=%d｜stderr=%s｜%v）",
			dev, r.ExitCode, strings.TrimSpace(r.Stderr), err)
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
	return subs, nil
}

// BtrfsSubvolIDs 返回 btrfs 子卷列表（忽略错误，仅取结果）。
func BtrfsSubvolIDs(dev string) []BtrfsSubvol {
	subs, _ := BtrfsSubvolIDsErr(dev)
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

// MdDetail mdadm --detail 解析结果（RAID 拓扑与健康状态）。
type MdDetail struct {
	Dev     string // /dev/md0
	Level   string // raid5 / raid1 / raid0 ...
	Members int    // Raid Devices（阵列成员总数）
	Active  int    // Active Devices（当前在线数）
	State   string // clean / degraded / recovering / resyncing ...
	UUID    string
	Size    string
}

// MdDetailOf 读取一个 md 阵列的拓扑与状态（只读）。
func MdDetailOf(dev string) MdDetail {
	d := MdDetail{Dev: dev}
	r, err := Run(15*time.Second, "mdadm", "--detail", dev)
	if err != nil {
		return d
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch key {
		case "Raid Level":
			d.Level = val
		case "Raid Devices":
			d.Members = atoiSafe(strings.Fields(val)[0])
		case "Active Devices":
			d.Active = atoiSafe(strings.Fields(val)[0])
		case "State":
			d.State = val // 形如 "clean" / "clean, degraded" / "clean, recovering"
		case "Array Size":
			d.Size = val
		case "UUID":
			d.UUID = val
		}
	}
	if d.State == "" && d.Level != "" {
		// 无冗余级别（raid0）没有 State 行，用成员数兜底
		d.State = "clean"
	}
	return d
}

// MdHealthy 判断阵列是否处于可接管状态。
// degraded / recovering / resyncing / resync 一律视为不健康——此时接管风险极高。
func MdHealthy(d MdDetail) bool {
	s := strings.ToLower(d.State)
	if strings.Contains(s, "degraded") || strings.Contains(s, "recovering") ||
		strings.Contains(s, "resyncing") || strings.Contains(s, "resync") ||
		strings.Contains(s, "rebuilding") {
		return false
	}
	if d.Members > 0 && d.Active > 0 && d.Active < d.Members {
		return false // 有成员掉线
	}
	return strings.Contains(s, "clean") || d.State == ""
}

// MdConfLine 返回可直接写进 mdadm.conf 的 ARRAY 行（官方推荐的生成方式）。
// 输出形如：ARRAY /dev/md/0 metadata=1.2 UUID=xxxx name=host:0
func MdConfLine(dev string) string {
	r, err := Run(15*time.Second, "mdadm", "--detail", "--scan", dev)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ARRAY") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// atoiSafe 解析整数，失败返回 0。
func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
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
	// 自动激活过的卷组：同一个 VG 的多个 LV 只激活一次，避免重复跑 vgchange
	activated := map[string]bool{}
	for _, lv := range lvs {
		if strings.HasPrefix(lv.VG, "trim_") {
			continue // 飞牛原生/已接管卷，跳过
		}
		if lv.LV == ReservedLV {
			continue // 群晖 12M 保留区：不是数据卷，不占候选位
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
		// 同时记下 PV 里有哪些是 md 阵列：区分"多块盘组成的 RAID"与"真跨盘卷"全靠它。
		var physDisks []string
		var mdDevs []string
		pvs := PVsOfVG(lv.VGUuid)
		for _, pv := range pvs {
			switch {
			case strings.HasPrefix(pv, "/dev/md"):
				mdDevs = append(mdDevs, pv)
				for _, m := range MdMembers(pv) {
					physDisks = append(physDisks, stripPartition(m))
				}
			case strings.HasPrefix(pv, "/dev/"):
				physDisks = append(physDisks, stripPartition(pv))
			}
		}
		physDisks = dedup(physDisks)
		c.PhysDisks = physDisks
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

		// —— 类型判定：单盘 Basic / RAID / 不支持 ——
		// 关键区分点：多块物理盘 ≠ RAID。在同一个 md 阵列上 = RAID 候选；
		// PV 散在多块盘或多个阵列上 = 真跨盘卷，仍然拒绝。
		c.Kind = KindUnsupported // 默认不支持，下面按证据放宽
		switch {
		case len(mdDevs) == 1 && len(physDisks) >= 2:
			d := MdDetailOf(mdDevs[0])
			c.MdDev, c.RaidLevel = mdDevs[0], d.Level
			c.RaidMembers, c.RaidActive, c.RaidHealth, c.RaidSize = d.Members, d.Active, d.State, d.Size
			if !MdHealthy(d) {
				c.Kind = KindUnsupported
				c.Notes = append(c.Notes, fmt.Sprintf(
					"阵列状态异常（%s，在线 %d/%d）：降级/重建中的阵列禁止接管，请先修复阵列",
					d.State, d.Active, d.Members))
			} else {
				c.Kind = KindRAID
				c.Notes = append(c.Notes, fmt.Sprintf(
					"检测到 %d 盘 %s 阵列（状态 %s）：RAID 接管将在 v0.6 提供，当前版本仅识别不接管",
					d.Members, d.Level, d.State))
			}
		case len(mdDevs) > 1 || len(pvs) > 1:
			c.Kind = KindUnsupported
			if len(mdDevs) > 1 {
				c.Notes = append(c.Notes, "卷组跨多个 md 阵列（"+strings.Join(mdDevs, ", ")+"），不支持接管")
			}
		default:
			// 单物理盘（群晖 Basic 常是单成员 md 阵列或直接分区）
			c.Kind = KindBasic
		}

		// —— 设备节点可用性 + 自动激活 ——
		// 真机上 5 块盘全部卡在这里：卷组没激活 → /dev/mapper/vgN-xxx 不是可用块设备 →
		// btrfs 报 "not a directory / can't access"。让用户自己去点按钮是推卸责任：
		// 激活只创建 dm 映射、不动数据，检测阶段自动做掉是安全的（幂等）。
		dev, devErr := ResolveBlockDev(c.DMPath, c.VG, c.LV)
		if devErr != "" && IsSynoDataVolume(c.VG, c.LV) && !activated[lv.VGUuid] {
			activated[lv.VGUuid] = true
			if err := VGActivate(lv.VGUuid); err == nil {
				c.Notes = append(c.Notes, "已自动激活该卷组（vgchange -ay <UUID>），仅创建设备映射、不改动数据")
				dev, devErr = ResolveBlockDev(c.DMPath, c.VG, c.LV)
			} else {
				c.Notes = append(c.Notes, fmt.Sprintf(
					"自动激活卷组失败：%v｜请在「磁盘接管」点『激活卷组』，或在 shell 执行：sudo vgchange -ay %s",
					err, lv.VGUuid))
			}
		}
		c.DevReady = devErr == ""
		c.DMPath = dev // 换成确实能用的那个路径，后续所有读取都以它为准
		if devErr != "" {
			c.Notes = append(c.Notes, devErr)
		} else {
			c.LVActive = true // 节点可用即视为已激活，比 lvs 的 attr 位可靠
		}

		// 子卷：不再要求 LVActive（真机上 lvs 的激活位未必可信，直接试读），
		// 读不到时把原因写进 notes——否则用户只看到空列表，无从下手。
		if c.DevReady {
			c.FS = FSType(c.DMPath)
			if c.FS == "btrfs" {
				subs, err := BtrfsSubvolIDsErr(c.DMPath)
				for _, sv := range subs {
					c.Subvols = append(c.Subvols, sv.Path)
				}
				if err != nil {
					c.Notes = append(c.Notes, "未读到 btrfs 子卷："+err.Error())
				} else if len(subs) == 0 {
					c.Notes = append(c.Notes, "该 btrfs 上没有任何子卷（不符合群晖布局，群晖数据卷应有 @syno）")
				}
			}
		}

		// 数据卷判定：认群晖的两种命名，不再写死 vg1/volume_1。
		// 关键：设备读不到就不能标"可接管"——否则用户点了接管才失败，比现在更糟。
		isData := IsSynoDataVolume(c.VG, c.LV)
		c.Adoptable = c.DevReady && c.FS == "btrfs" && isData && len(physDisks) == 1
		if !isData {
			c.Notes = append(c.Notes, fmt.Sprintf(
				"%s/%s 不符合群晖数据卷命名（应为 vgN/volume_N 或 vgN/lv），已排除", c.VG, c.LV))
		}
		if c.FS == "ext4" {
			c.Notes = append(c.Notes, "ext4 卷：可挂载读取，但“接管”流程仅支持 btrfs")
		}
		if !c.LVActive && c.FS == "" {
			c.Notes = append(c.Notes, "逻辑卷未激活，无法读取文件系统类型（可到「磁盘接管」点『激活卷组』）")
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
