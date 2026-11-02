package commander

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DiskOfDMName 通过 sysfs 反查某个 dm 设备背后的物理盘名（如 /dev/sdd）。
//
// 为什么需要它：接管记录 adopted.json 里存的是**接管当时**的盘符，而这块机器
// 重启后盘符会漂移（真机实测 2026-10-31：/vol3 那块盘接管时是 sde，现在实际
// 是 sdd）。直接拿记录里的 Disk 去展示，列表里就会出现两个一模一样的 sde、
// 而真正的 sdd 凭空消失——用户看到的就是"少了一块盘"。
// dm 映射名由卷组 UUID 派生，跟着盘走，不受盘符影响，从它反查才准。
func DiskOfDMName(dmName string) string {
	target := ""
	names, _ := filepath.Glob("/sys/block/dm-*/dm/name")
	for _, p := range names {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == dmName {
			target = filepath.Dir(filepath.Dir(p)) // /sys/block/dm-N
			break
		}
	}
	if target == "" {
		return ""
	}
	cur := target
	for i := 0; i < 4; i++ {
		slaves, _ := filepath.Glob(filepath.Join(cur, "slaves", "*"))
		if len(slaves) == 0 {
			break
		}
		cur = slaves[0]
	}
	base := filepath.Base(cur)
	// 走到分区（sda3 / nvme0n1p3）时去掉分区号，还原成整盘名
	disk := regexp.MustCompile(`^(nvme\d+n\d+)p\d+$|^([a-z]+)\d+$`).ReplaceAllString(base, "$1$2")
	if disk == "" {
		disk = base
	}
	return "/dev/" + disk
}

// DiskOfVG 反查某个已接管卷组当前的物理盘（按飞牛命名 trim_xxx-0）。
// 查不到返回空串，调用方自行回退到记录里的旧值。
func DiskOfVG(newVG string) string {
	if newVG == "" {
		return ""
	}
	return DiskOfDMName(newVG + "-0")
}
