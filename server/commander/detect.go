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
	Adopted   bool     `json:"adopted"`   // 已由本工具接管（api 层按接管记录打标）——终态，不可再选
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
	KindDirect      = "direct"      // btrfs 直接建在 md 阵列上，中间没有 LVM 层
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

// DevSize 查询单个设备的容量（lsblk 的 JSON 是树形的，md 之类嵌在 children 里，
// 顶层 BlockDevices 拿不到，只能单独问一次）。
func DevSize(dev string) string {
	r, err := Run(10*time.Second, "lsblk", "-dn", "-o", "SIZE", dev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// AllPVs 返回当前全部物理卷路径（用来判断某个 md 阵列有没有被 LVM 用掉）。
func AllPVs() []string {
	r, err := Run(10*time.Second, "pvs", "--noheadings", "-o", "pv_name")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ActiveMdDevs 返回当前已组装的 md 阵列设备路径（/dev/mdN）。
func ActiveMdDevs() []string {
	names, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n.Name(), "md") {
			continue
		}
		dev := "/dev/" + n.Name()
		if st, err := os.Stat(dev); err == nil && isBlockDevMode(st.Mode()) {
			out = append(out, dev)
		}
	}
	return out
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
// 注意：设备有多个挂载点时返回第一个（历史救援脚本的只读残留可能排在
// 接管挂载前面）——需要完整列表用 MountPointsOf。
func MountPointOf(dev string) string {
	if mps := MountPointsOf(dev); len(mps) > 0 {
		return mps[0]
	}
	return ""
}

// MountPointsOf 返回设备当前全部挂载点。
// btrfs 允许同一文件系统同时挂多处：接管挂载（/volN）与历史救援脚本的
// 残留挂载（如 /mnt/t2）并存是真实场景，只看第一个会误判"挂载点不一致"。
func MountPointsOf(dev string) []string {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	targets := map[string]bool{dev: true}
	if real, e := filepath.EvalSymlinks(dev); e == nil {
		targets[real] = true
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && targets[f[0]] {
			out = append(out, strings.ReplaceAll(f[1], "\\040", " "))
		}
	}
	return out
}

// IsMountpoint 判断某目录当前是否为挂载点（比对 /proc/self/mounts 挂载点列）。
// 与 MountPointsOf 相反：那个按设备查挂载点，这个按目录查是否被挂载。
func IsMountpoint(dir string) bool {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == dir {
			return true
		}
	}
	return false
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

// MountOptionsOf 返回某挂载点当前的挂载选项（/proc/self/mounts 第 4 字段）。
// 挂载点不存在返回空串。注意 /proc/mounts 会把空格转义成 \040，这里已还原。
func MountOptionsOf(mountPoint string) string {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && strings.ReplaceAll(f[1], "\\040", " ") == mountPoint {
			return f[3]
		}
	}
	return ""
}

// MountHasOption 判断某挂载点是否带指定挂载选项。
// 按逗号切分后逐项精确比对——不能用 strings.Contains，否则 "trimacl"
// 会被 "notrimacl" 之类的前缀命中（挂载选项里 negatoin 很常见）。
func MountHasOption(mountPoint, opt string) bool {
	opts := MountOptionsOf(mountPoint)
	if opts == "" {
		return false
	}
	for _, o := range strings.Split(opts, ",") {
		if o == opt {
			return true
		}
	}
	return false
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
	} else if !st.IsDir() && !isBlockDevMode(st.Mode()) {
		return nil, fmt.Errorf("%s 不是块设备也不是目录（%s）：设备节点异常，请激活卷组后重试", dev, st.Mode().Type())
	}
	// 大卷（十几 T）上 subvolume list 可能较慢，超时给足
	r, err := Run(90*time.Second, "btrfs", "subvolume", "list", dev)
	if err != nil {
		// 群晖的默认子卷（@syno）在文件系统里**没有目录名**，还有一些残留旧树也是，
		// 老版本 btrfs-progs 会对它们往 stderr 打警告、甚至以非 0 退出——但 stdout
		// 里已经列出来的子卷仍然可信。真机 2026-10-31 就是这种盘：重建子卷后留下
		// 无名旧树，如果这里硬报错，整个接管流程会因为"列子卷失败"而卡死。
		// 因此：只要 stdout 能解析出子卷就继续；一条都解析不出来才算真失败。
		if strings.TrimSpace(r.Stdout) == "" {
			return nil, fmt.Errorf("btrfs subvolume list %s 失败（exit=%d｜stderr=%s｜%v）",
				dev, r.ExitCode, strings.TrimSpace(r.Stderr), err)
		}
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
	if len(subs) == 0 && err != nil {
		return nil, fmt.Errorf("btrfs subvolume list %s 没有列出任何子卷（exit=%d｜stderr=%s）",
			dev, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return subs, nil
}

// dmNameReplacer dm 设备名的 UUID 字符替换（与 adopt 包的 trimDMName 同规则）。
var dmNameReplacer = strings.NewReplacer(":", "_", "-", "_")

// DmNameFor 由 md 阵列 UUID 生成飞牛原生命名风格的 dm 设备名 trim_<uuid>-0。
// 放在 commander 以便探测回退使用（adopt 包依赖 commander，反向引用会成环）。
func DmNameFor(mdUUID string) string {
	return "trim_" + dmNameReplacer.Replace(mdUUID) + "-0"
}

// BtrfsSubvolsForDev 读子卷的完整策略（检测与计划生成统一入口）：
//  1. 自动挂载法（v6.2 唯一常规途径）；
//  2. 失败则查该设备自身是否已有挂载点（手工 ro 挂载等），直接复用列子卷；
//  3. 再查本工具的 dm-linear 包装设备是否已挂载（直建盘接管后的再探测场景）。
//
// 背景：同一个 btrfs 已挂载时，从另一设备路径再挂会 EBUSY（already mounted）。
// 真机场景：md2 经 dm 包装挂在 /vol2 后，检测面板再探 /dev/md2 就撞车——
// 盘和树根都是好的，必须复用现有挂载点，而不是误报"树根损坏"。
func BtrfsSubvolsForDev(dev string) ([]BtrfsSubvol, string, error) {
	subs, via, err := BtrfsSubvolsAuto(dev)
	if err == nil {
		return subs, via, nil
	}
	for _, mp := range MountPointsOf(dev) {
		if s, e := BtrfsSubvolIDsErr(mp); e == nil {
			return s, "existing-mount", nil
		}
	}
	if d := MdDetailOf(dev); d.UUID != "" {
		dm := "/dev/mapper/" + DmNameFor(d.UUID)
		for _, mp := range MountPointsOf(dm) {
			if s, e := BtrfsSubvolIDsErr(mp); e == nil {
				return s, "dm-mount", nil
			}
		}
	}
	return nil, "", err
}

// IsSynoSubvolName 判断子卷 path 是否为群晖数据根（@syno）。
//
// 真机实锤（2026-10-30，md2）：数据根子卷名可能是怪名——@syno、@\syno\、@//syno/
// 等（含斜杠/反斜杠变体）。btrfs 子卷名是任意字节串，on-disk 存什么就是什么；
// 只有走路径访问时斜杠才会被拆解，所以工具全程用子卷 ID 操作，匹配时把分隔符
// 剥掉再比较即可唯一识别（对干净名 @syno 无影响）。
func IsSynoSubvolName(path string) bool {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' {
			return -1
		}
		return r
	}, path) == "@syno"
}

// BtrfsDefaultSubvolID 读挂载点当前默认子卷的 ID（btrfs subvolume get-default）。
// 用途：接管选数据根时，默认子卷指着的那个就是"正在服役"的正主——
// 盘上若同时存在干净名/怪名两个候选，优先认 default，避免把默认切到旧副本。
func BtrfsDefaultSubvolID(mnt string) (int, error) {
	r, err := Run(15*time.Second, "btrfs", "subvolume", "get-default", mnt)
	if err != nil {
		return 0, fmt.Errorf("btrfs subvolume get-default %s 失败（exit=%d｜stderr=%s）",
			mnt, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	var id int
	if _, e := fmt.Sscanf(strings.TrimSpace(r.Stdout), "ID %d", &id); e != nil || id <= 0 {
		return 0, fmt.Errorf("get-default 输出无法解析: %q", strings.TrimSpace(r.Stdout))
	}
	return id, nil
}

// BtrfsSubvolsViaMount 只读挂载 btrfs 的 FS tree 后列出子卷。
// 第二个返回值表示是否动用了备份树根（说明该文件系统树根有损坏）。
//
// 真机实测（fnOS + btrfs-progs v6.2）：直接对块设备执行
// `btrfs subvolume list /dev/dm-N` 会报 "ERROR: not a directory / can't access"，
// 哪怕 stat -L 确认它确实是 block special file——同一条命令对 /dev/md2 这种裸
// 块设备也一样失败。结论：v6.2 的 subvolume list 要的是**挂载点**，不是设备。
//
// 必须挂 subvolid=5（FS tree）而不是默认子卷：群晖把 default 设成 @syno，
// 挂默认子卷只能看到 @syno 内部，列不出 @syno 本身。
func BtrfsSubvolsViaMount(dev string) ([]BtrfsSubvol, bool, error) {
	dir, err := os.MkdirTemp("", "fn-syno-probe-")
	if err != nil {
		return nil, false, fmt.Errorf("创建临时挂载点失败: %w", err)
	}
	defer os.Remove(dir) // 卸载后目录为空，可以删掉

	rescued := false
	// btrfs 多设备文件系统必须先让内核注册设备，否则单独挂其中一个会报
	// "can't read superblock"。scan 是幂等的，失败也不影响后续尝试。
	Run(30*time.Second, "btrfs", "device", "scan", dev)

	// 必须带 -t btrfs：不指定的话 mount 会自己探测所有 fs 类型，失败时只给一句
	// "can't read superblock"，真正的 btrfs 报错被吞掉，排查无从下手。
	r, err := Run(30*time.Second, "mount", "-t", "btrfs", "-o", "ro,subvolid=5", dev, dir)
	if err != nil {
		// 树根损坏时（真机实测：corrupt leaf / couldn't read tree root / open_ctree failed）
		// 用备份 root 再试一次——飞牛官方的恢复挂载命令用的也是 usebackuproot。
		// 只读 + 不改盘，安全。
		r2, err2 := Run(30*time.Second, "mount", "-t", "btrfs",
			"-o", "ro,usebackuproot,subvolid=5", dev, dir)
		if err2 != nil {
			// 先查是不是群晖私有标记（真机确认：DSM 7 给共享文件夹子卷写私有 root flag
			// 0x400000000，主线内核 5.4+ 的 tree-checker 拒绝 → open_ctree -5，对外只
			// 表现成 "can't read superblock"）。这种情况下盘和数据都没坏，不能再说"树根损坏"。
			if flag := SynoPrivateFlagHint(); flag != "" {
				return nil, false, fmt.Errorf(
					"内核拒绝挂载（%s）。这不是树根损坏：该标记是群晖 DSM 7 写入的私有 root flag，"+
						"主线内核 5.4+ 校验不通过就拒绝整个挂载。盘和数据都没坏——用户态 btrfs restore "+
						"不做此校验，可直接读出数据；要原生挂载需给内核 btrfs 模块打补丁（tree-checker 放行）",
					flag)
			}
			// EBUSY 不是树根问题：同一 btrfs 已挂在别处（接管后的 dm 包装、手工 ro 残留等），
			// 从另一设备路径重复挂载必撞 already mounted。如实说明，避免吓用户。
			if r.ExitCode == 32 || strings.Contains(r.Stderr, "already mounted") ||
				strings.Contains(r2.Stderr, "already mounted") {
				return nil, false, fmt.Errorf(
					"挂载 %s 失败：already mounted or mount point busy（exit=%d｜stderr=%s）。"+
						"该文件系统已挂载在别处，同一 btrfs 不允许从另一设备路径重复挂载——"+
						"这不是损坏，应复用现有挂载点读取子卷",
					dev, r.ExitCode, strings.TrimSpace(r.Stderr))
			}
			return nil, false, fmt.Errorf("只读挂载 %s 失败（exit=%d｜stderr=%s）；"+
				"用备份树根重试也失败（exit=%d｜stderr=%s）：该 btrfs 树根已损坏，"+
				"建议先备份数据再修（btrfs check --readonly）",
				dev, r.ExitCode, strings.TrimSpace(r.Stderr),
				r2.ExitCode, strings.TrimSpace(r2.Stderr))
		}
		rescued = true
	}
	defer func() {
		// 务必卸载：残留挂载会让后续接管失败，也会挡住磁盘弹出
		if _, err := Run(30*time.Second, "umount", dir); err != nil {
			Run(30*time.Second, "umount", "-l", dir) // 惰性卸载兜底
		}
	}()
	subs, err := BtrfsSubvolIDsErr(dir)
	return subs, rescued, err
}

// BtrfsSubvolsAuto 先用挂载法读子卷（v6.2 上唯一可行），读不到再退回块设备法。
// 返回子卷、实际用的方式（mount / mount-backuproot / dev）、以及都失败时的错误。
func BtrfsSubvolsAuto(dev string) ([]BtrfsSubvol, string, error) {
	subs, rescued, err := BtrfsSubvolsViaMount(dev)
	if err == nil {
		if rescued {
			return subs, "mount-backuproot", nil
		}
		return subs, "mount", nil
	}
	mErr := err
	if subs2, err2 := BtrfsSubvolIDsErr(dev); err2 == nil {
		return subs2, "dev", nil
	}
	return nil, "", fmt.Errorf("只读挂载法：%v；块设备法也失败", mErr)
}

// SynoPrivateFlagHint 从内核日志里找群晖私有 root flag 的证据。
//
// 真机确认的根因：群晖 DSM 7 给共享文件夹子卷的 root item 写入私有 flag
// 0x400000000，主线内核 5.4+ 的 tree-checker 校验不认识就判定"元数据损坏"，
// 拒绝整个挂载（open_ctree -5），对外只表现成 mount 的 "can't read superblock"。
// 报错形如：invalid root flags, have 0x400000000 expect mask 0x1000000000001
//
// 必须在刚尝试过挂载之后调用（内核日志是全局的，只在失败后立即读才可靠）。
// 返回匹配的报错片段（含 flag 值），没有证据返回空串。
func SynoPrivateFlagHint() string {
	r, err := Run(15*time.Second, "dmesg")
	if err != nil {
		return ""
	}
	hint := ""
	for _, line := range strings.Split(r.Stdout, "\n") {
		if i := strings.Index(line, "invalid root flags, have "); i >= 0 {
			// 取最后一条匹配：dmesg 可能留有历史失败记录，最新的一次才是本次的
			hint = strings.TrimSpace(line[i:])
		}
	}
	return hint
}

// BtrfsRestoreRoots 用 btrfs restore -l（纯用户态）列出文件系统的 tree root。
//
// 用途：内核拒绝挂载（群晖私有 flag / 树根损坏）时，restore 走用户态、
// 不做内核那套校验，仍能列出完整 tree root——这是"数据还能救"的直接证据。
// 只读，不写盘。
func BtrfsRestoreRoots(dev string) ([]int, error) {
	if _, err := exec.LookPath("btrfs"); err != nil {
		return nil, fmt.Errorf("系统缺少 btrfs-progs：%v", err)
	}
	r, err := Run(90*time.Second, "btrfs", "restore", "-l", dev)
	if err != nil {
		return nil, fmt.Errorf("btrfs restore -l %s 失败（exit=%d｜stderr=%s）",
			dev, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	var ids []int
	for _, line := range strings.Split(r.Stdout, "\n") {
		var id int
		if _, e := fmt.Sscanf(line, "ID %d", &id); e == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("btrfs restore -l 未列出任何 tree root（输出: %s）",
			strings.TrimSpace(r.Stdout))
	}
	return ids, nil
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
					"检测到 %d 盘 %s 阵列（状态 %s）：RAID 接管尚未实现，当前版本仅识别不接管",
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
		// subvolReadOK / hasSyno 同时是「可接管」的必要条件：接管执行时必须
		// 挂载 FS tree 并把默认子卷切到 @syno，挂载被拒（如群晖私有 root flag、
		// 树根损坏）或找不到 @syno 的盘，接管必然在中途失败——就不能标可接管。
		subvolReadOK, hasSyno := false, false
		if c.DevReady {
			c.FS = FSType(c.DMPath)
			if c.FS == "btrfs" {
				// 统一入口：自动挂载失败时回退复用设备已有挂载点 / dm 包装挂载点。
				// 真机场景：md2 已 dm 包装挂 /vol2，再从 /dev/md2 挂就 EBUSY——
				// 若直接报错会被误读成树根损坏，所以必须走 ForDev 的多级回退。
				subs, via, err := BtrfsSubvolsForDev(c.DMPath)
				var synoName string
				for _, sv := range subs {
					c.Subvols = append(c.Subvols, sv.Path)
					if synoName == "" && IsSynoSubvolName(sv.Path) {
						hasSyno = true
						synoName = sv.Path
					}
				}
				if hasSyno && synoName != "@syno" {
					c.Notes = append(c.Notes, fmt.Sprintf(
						"✔ 数据根子卷名为群晖怪名 %q（含斜杠的命名残留，非显示错误）——"+
							"工具全程按子卷 ID 操作，接管不受影响", synoName))
				}
				if err != nil {
					c.Notes = append(c.Notes, "未读到 btrfs 子卷："+err.Error())
					c.Notes = append(c.Notes, rescueRootsNote(c.DMPath)...)
				} else {
					subvolReadOK = true
					if via == "mount-backuproot" {
						c.Notes = append(c.Notes,
							"⚠ 该 btrfs 的**主树根已损坏**，本次是用备份树根（usebackuproot）挂载才读到子卷。"+
								"数据可能不完整，接管前请先自行备份重要文件")
					} else if via == "mount" {
						c.Notes = append(c.Notes,
							"✔ 子卷经只读挂载（subvolid=5）读取：btrfs-progs v6.2 的 subvolume list 只认挂载点、"+
								"不认块设备路径（传设备会报 not a directory），先挂载再列出是正常流程，不是错误")
					} else if via == "existing-mount" || via == "dm-mount" {
						c.Notes = append(c.Notes,
							"✔ 子卷复用该文件系统现有挂载点读取（同一 btrfs 不允许从另一设备路径重复挂载，"+
								"探测撞 EBUSY 时回退复用是正常流程，不是错误）")
					}
					if len(subs) == 0 {
						c.Notes = append(c.Notes, "该 btrfs 上没有任何子卷（不符合群晖布局，群晖数据卷应有 @syno）")
					}
				}
			}
		}

		// 数据卷判定：认群晖的两种命名，不再写死 vg1/volume_1。
		// 关键：设备读不到、子卷读不到、找不到 @syno，都不能标"可接管"——
		// 否则用户点了接管才失败，比现在更糟。
		isData := IsSynoDataVolume(c.VG, c.LV)
		if !isData {
			c.Notes = append(c.Notes, fmt.Sprintf(
				"%s/%s 不符合群晖数据卷命名（应为 vgN/volume_N 或 vgN/lv），已排除", c.VG, c.LV))
		}
		c.Adoptable = c.DevReady && c.FS == "btrfs" && isData && len(physDisks) == 1 &&
			subvolReadOK && hasSyno
		if !c.Adoptable && c.DevReady && c.FS == "btrfs" && isData && len(physDisks) == 1 {
			switch {
			case !subvolReadOK:
				c.Notes = append(c.Notes,
					"暂不可接管：接管需要只读挂载该卷并定位 @syno 子卷（用于切换默认子卷），挂载被拒就无从执行")
			case !hasSyno:
				c.Notes = append(c.Notes,
					"暂不可接管：该卷上没有 @syno 数据子卷，不符合群晖数据卷布局，无法切换默认子卷")
			}
		}
		if c.FS == "ext4" {
			c.Notes = append(c.Notes, "ext4 卷：可挂载读取，但“接管”流程仅支持 btrfs")
		}
		if !c.LVActive && c.FS == "" {
			c.Notes = append(c.Notes, "逻辑卷未激活，无法读取文件系统类型（可到「磁盘接管」点『激活卷组』）")
		}
		candidates = append(candidates, c)
	}
	candidates = append(candidates, detectDirectBtrfs(disks, candidates)...)
	return candidates, disks, nil
}

// detectDirectBtrfs 找出「btrfs 直接建在 md 阵列上、中间没有 LVM 层」的盘。
//
// 真机实测（用户的机器）：sdc→md2(10.9T)、sdd→md3(14.5T) 就是这种布局，
// 群晖 Basic 的另一种做法。它们**不会出现在 lvs 里**，所以主循环扫不到，
// 用户只会看到"我插了 7 块，界面只有 5 块"。这里单独扫一遍把它们列出来。
//
// v0.6：布局健康（阵列 clean + 读得到 @syno 子卷）的直建盘直接标可接管。
// 直建布局没有 LVM 层，接管不需要 vgrename/lvrename，比 Basic 路径还少几步
// （见 adopt.BuildPlanDirect）。
func detectDirectBtrfs(disks []BlockDevice, existing []SynoCandidate) []SynoCandidate {
	pvSet := map[string]bool{}
	for _, p := range AllPVs() {
		pvSet[p] = true
	}
	// 已经被主循环收进来的设备（LVM 卷）不重复列
	seen := map[string]bool{}
	for _, c := range existing {
		seen[c.DMPath] = true
	}
	sizeOf := map[string]string{}
	diskInfo := map[string]BlockDevice{}
	for _, d := range disks {
		sizeOf[d.Name] = d.Size
		if d.Type == "disk" {
			diskInfo[d.Name] = d
		}
	}

	var out []SynoCandidate
	for _, md := range ActiveMdDevs() {
		if pvSet[md] || seen[md] {
			continue
		}
		fs := FSType(md)
		if fs != "btrfs" {
			continue
		}
		members := MdMembers(md)
		var phys []string
		for _, m := range members {
			phys = append(phys, stripPartition(m))
		}
		phys = dedup(phys)

		// 阵列健康：degraded / rebuilding 的直建阵列禁止接管（与 RAID 路径同一标准）
		d := MdDetailOf(md)
		healthy := MdHealthy(d)

		c := SynoCandidate{
			VG:        "—", // 没有卷组，这是关键差异
			LV:        "—",
			DMPath:    md,
			MdDev:     md,
			LVSize:    DevSize(md), // lsblk 树形输出里没有 md，必须单独查
			FS:        fs,
			DevReady:  true, // md 阵列本身就是块设备
			Kind:      KindDirect,
			PhysDisks: phys,
			Subvols:   []string{},
			Notes:     []string{},
		}
		c.RaidLevel, c.RaidMembers = d.Level, d.Members
		c.RaidActive, c.RaidHealth, c.RaidSize = d.Active, d.State, d.Size
		if len(phys) == 1 {
			c.Disk = phys[0]
			if d, ok := diskInfo[c.Disk]; ok {
				c.Model, c.Serial = d.Model, d.Serial
			}
		}
		// 子卷照样读：确认它是群晖布局（应有 @syno），顺便让用户看到内容还在。
		// 与主循环同一标准：读不到子卷 / 没有 @syno 就不能标可接管。
		// 用 ForDev 而非 Auto：直建盘接管后经 dm 包装挂 /volN，再从 /dev/mdN
		// 自动挂载必撞 EBUSY，需回退复用现有挂载点（否则误报树根损坏）。
		subvolReadOK, hasSyno := false, false
		subs, via, err := BtrfsSubvolsForDev(md)
		var synoName string
		for _, sv := range subs {
			c.Subvols = append(c.Subvols, sv.Path)
			if synoName == "" && IsSynoSubvolName(sv.Path) {
				hasSyno = true
				synoName = sv.Path
			}
		}
		if hasSyno && synoName != "@syno" {
			c.Notes = append(c.Notes, fmt.Sprintf(
				"✔ 数据根子卷名为群晖怪名 %q（含斜杠的命名残留，非显示错误）——"+
					"工具全程按子卷 ID 操作，接管不受影响", synoName))
		}
		if err != nil {
			c.Notes = append(c.Notes, "未读到 btrfs 子卷："+err.Error())
			c.Notes = append(c.Notes, rescueRootsNote(md)...)
		} else {
			subvolReadOK = true
			if via == "mount-backuproot" {
				c.Notes = append(c.Notes,
					"⚠ 该 btrfs 的**主树根已损坏**，本次是用备份树根（usebackuproot）挂载才读到子卷。"+
						"数据可能不完整，接管前请先自行备份重要文件")
			} else if via == "existing-mount" || via == "dm-mount" {
				c.Notes = append(c.Notes,
					"✔ 子卷复用该文件系统现有挂载点读取（探测不再重复挂载，避免 EBUSY 误报）")
			}
			if len(subs) == 0 {
				c.Notes = append(c.Notes, "该 btrfs 上没有任何子卷（不符合群晖布局，群晖数据卷应有 @syno）")
			}
		}
		c.Adoptable = healthy && len(phys) >= 1 && subvolReadOK && hasSyno
		switch {
		case !healthy:
			c.Notes = append(c.Notes, fmt.Sprintf(
				"阵列状态异常（%s，在线 %d/%d）：降级/重建中的阵列禁止接管，请先修复阵列",
				d.State, d.Active, d.Members))
		case c.Adoptable:
			c.Notes = append(c.Notes,
				"btrfs 直接建在阵列 "+md+" 上，中间没有 LVM 层（群晖的另一种布局）："+
					"已支持接管——无需 LVM 改名，直接切换默认子卷、挂载并注册")
		default:
			c.Notes = append(c.Notes,
				"btrfs 直接建在阵列 "+md+" 上，中间没有 LVM 层（群晖的另一种布局）："+
					"当前不满足接管条件（见上方原因）")
		}
		out = append(out, c)
	}
	return out
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

// rescueRootsNote 子卷读不到时，用纯用户态的 btrfs restore -l 再试一次。
//
// 内核拒绝挂载（群晖私有 root flag）不等于数据不可读：restore 走用户态、
// 不做内核 tree-checker 那套校验。能列出 tree root 就是"数据完好可救"的
// 直接证据，必须告诉用户，否则界面上只剩一句报错，用户会以为盘废了。
// 只读、不写盘。失败则返回空（不额外添乱）。
func rescueRootsNote(dev string) []string {
	ids, err := BtrfsRestoreRoots(dev)
	if err != nil || len(ids) == 0 {
		return nil
	}
	shown := ids
	if len(shown) > 6 {
		shown = shown[:6]
	}
	idStr := make([]string, len(shown))
	for i, id := range shown {
		idStr[i] = strconv.Itoa(id)
	}
	more := ""
	if len(ids) > 6 {
		more = fmt.Sprintf(" 等 %d 个", len(ids))
	}
	return []string{fmt.Sprintf(
		"✔ 但 btrfs restore（用户态，不受内核校验影响）能列出 %d 个 tree root（ID: %s%s）："+
			"文件系统结构完好、数据可救，可用数据抢救（btrfs restore）直接提取文件",
		len(ids), strings.Join(idStr, "/"), more)}
}
