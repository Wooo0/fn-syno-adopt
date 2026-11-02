package commander

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LVM 自动归档目录。每次 vgrename / lvrename 执行前，LVM 都会把改名前那份
// 元数据存进这里（/etc/lvm/archive/<vgname>_<seq>-<ts>.vg）。
//
// 这是"接管中断后原名是什么"的唯一可靠来源：接管一旦在改名之后失败，
// 卷组名已经是 trim_xxx、逻辑卷名已经是 0，而 adopted.json 还没写——
// 盘上剩下的信息里没有任何一处记得它原来叫 vg4/volume_6。
var archDir = "/etc/lvm/archive"

// LVM 文本格式的层级全靠缩进，**必须严格按缩进匹配**：
//
//	vg4 {                      ← 顶格：卷组块（只有它顶格）
//		id = "..."
//		physical_volumes {     ← 一个 tab：保留区块，不是卷名
//			pv0 {              ← 两个 tab
//		logical_volumes {      ← 一个 tab
//			volume_6 {         ← 两个 tab：逻辑卷
//
// 踩过的坑（真机 2026-10-31，代价是改坏了一块盘）：卷组块的正则原先写成
// `^\t([\w.-]+) \{`，于是第一个匹配到的其实是 `physical_volumes {`——
// 回滚时照着它执行 `vgrename <uuid> physical_volumes`，把卷组真的改名成了
// physical_volumes，检测面板随即认不出这块盘。解析归档这种"用来还原名字"的
// 证据源，错一个字符就是直接写坏盘，所以这里必须与格式严格对齐。
var (
	vgBlockRE = regexp.MustCompile(`(?m)^([A-Za-z0-9_+.-]+) \{`)     // 顶格 = 卷组
	lvBlockRE = regexp.MustCompile(`(?m)^\t\t([A-Za-z0-9_+.-]+) \{`) // 两个 tab = 逻辑卷
)

// 群晖的卷组名恒为 vgN（vg1 / vg2 / vg1001…），逻辑卷名恒为 volume_N 或 lv。
// 解析结果必须落在这个范围里才允许用于改名——宁可回滚失败，也不能把盘改成
// 一个莫名其妙的区块名。
var (
	synoVGNameRE = regexp.MustCompile(`^vg\d+$`)
	synoLVNameRE = regexp.MustCompile(`^(volume_\d+|lv)$`)
)

// IsSynoLVName 判断逻辑卷名是否符合群晖命名（volume_N / lv）。
// 导出给 adopt 包复用，避免两处各写一份正则、各自漂移。
func IsSynoLVName(name string) bool { return synoLVNameRE.MatchString(name) }

// IsSynoVGName 判断卷组名是否符合群晖命名（vgN）。
func IsSynoVGName(name string) bool { return synoVGNameRE.MatchString(name) }

// VGOrigin 某个卷组在改名前的原始名字。
type VGOrigin struct {
	VG   string   // 原始卷组名，如 vg4 / vg1001
	LVs  []string // 原始逻辑卷名，如 volume_6 / lv
	From string   // 证据来源的 archive 文件（排障可查）
}

// OriginOf 从 LVM 归档里回溯某卷组 UUID 的原始名字。
//
// 同一卷组会有多份归档（群晖建卷、我们改名前后各一份），**取时间最早的那份**
// 才拿到未改名前的名字。找不到归档（比如盘是在别处改的名）返回 false，
// 由调用方降级为"让用户手填原名"。
func OriginOf(vgUUID string) (VGOrigin, bool) {
	if vgUUID == "" {
		return VGOrigin{}, false
	}
	entries, err := os.ReadDir(archDir)
	if err != nil {
		return VGOrigin{}, false
	}
	needle := `id = "` + vgUUID + `"`

	type archFile struct {
		path string
		mod  int64
	}
	var hits []archFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".vg") {
			continue
		}
		p := filepath.Join(archDir, e.Name())
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		if !strings.Contains(string(b), needle) {
			continue
		}
		info, ierr := e.Info()
		var mod int64
		if ierr == nil {
			mod = info.ModTime().UnixNano()
		}
		hits = append(hits, archFile{path: p, mod: mod})
	}
	if len(hits) == 0 {
		return VGOrigin{}, false
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod < hits[j].mod })

	// 从最早的一份开始试，第一份格式不对就往后翻，而不是直接放弃。
	// 理由：v0.8.5 那次事故之后，某卷组"最早的一份归档"可能已经是被改错的
	// 名字（physical_volumes），但更早/更晚的归档里仍留着正确名字。
	for _, h := range hits {
		if o, ok := parseArchive(h.path); ok {
			return o, true
		}
	}
	return VGOrigin{}, false
}

// parseArchive 解析单份归档文件；名字不符合群晖命名就返回 false（交给下一份）。
func parseArchive(path string) (VGOrigin, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return VGOrigin{}, false
	}
	text := string(b)

	o := VGOrigin{From: path}
	if m := vgBlockRE.FindStringSubmatch(text); len(m) == 2 {
		o.VG = m[1]
	}
	// 逻辑卷名：只在 logical_volumes 块内取，避免把 physical_volumes 的 pv0 当卷名。
	if idx := strings.Index(text, "logical_volumes"); idx >= 0 {
		for _, m := range lvBlockRE.FindAllStringSubmatch(text[idx:], -1) {
			if len(m) == 2 && synoLVNameRE.MatchString(m[1]) {
				o.LVs = append(o.LVs, m[1])
			}
		}
	}
	// 最后一道闸：名字格式不像群晖卷就当作"没解析出来"，让调用方转人工。
	// 回滚会直接用这个名字去 vgrename，错一次就是永久性改名。
	if !synoVGNameRE.MatchString(o.VG) {
		return VGOrigin{}, false
	}
	return o, true
}
