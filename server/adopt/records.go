// 接管记录：只有本应用接管的卷才能被回滚。
//
// 为什么必须有这份记录：飞牛原生存储空间的卷组同样叫 trim_<uuid>、逻辑卷同样叫 0，
// 仅凭 "trim_ 前缀 + LV=0" 无法区分"我们接管的群晖盘"和"飞牛自己创建的存储空间"。
// 若把飞牛原生卷也列进回滚列表，用户一键点下去就是灾难。
// 因此：接管成功写记录，回滚列表只读记录，回滚成功删记录。
package adopt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const recordsFile = "adopted.json"

var recMu sync.Mutex

// Record 一次成功接管的记录（回滚的唯一依据）。
type Record struct {
	VGUuid     string `json:"vgUuid"`     // 卷组 UUID：全程按 UUID 定位，重名免疫
	OldVG      string `json:"oldVG"`      // 原名（群晖盘恒为 vg1）
	NewVG      string `json:"newVG"`      // trim_<uuid>
	OldLV      string `json:"oldLV"`      // 原名 volume_1
	VolID      int    `json:"volId"`      // 存储空间编号
	MountPoint string `json:"mountPoint"` // /volN
	MUUID      string `json:"muuid"`      // mount 表 uuid
	Disk       string `json:"disk"`       // 物理盘（展示用）
	LVSize     string `json:"lvSize"`
	Time       string `json:"time"` // 接管时间
	// MdUUIDs 卷底层 md 阵列的 UUID（用于回滚时从 mdadm.conf 移除我们写过的行）。
	// v0.5.x 的老记录没有这个字段，反序列后为空 → 回滚跳过移除，向后兼容。
	MdUUIDs []string `json:"mdUuids,omitempty"`
}

// recordsPath 记录文件绝对路径。
func recordsPath() string { return filepath.Join(StateDir, recordsFile) }

// LoadRecords 读取全部接管记录（文件不存在时返回空列表，不报错）。
func LoadRecords() ([]Record, error) {
	recMu.Lock()
	defer recMu.Unlock()
	return loadRecordsLocked()
}

func loadRecordsLocked() ([]Record, error) {
	b, err := os.ReadFile(recordsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []Record{}, nil
		}
		return nil, err
	}
	var out []Record
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("接管记录文件解析失败（%s）: %w", recordsPath(), err)
	}
	if out == nil {
		out = []Record{}
	}
	return out, nil
}

// SaveRecord 追加/更新一条接管记录（按 vgUuid 去重，原子写）。
func SaveRecord(r Record) error {
	recMu.Lock()
	defer recMu.Unlock()
	list, err := loadRecordsLocked()
	if err != nil {
		// 记录文件损坏不应阻断已完成的操作降级为覆盖写一条
		list = []Record{}
	}
	replaced := false
	for i := range list {
		if list[i].VGUuid == r.VGUuid {
			list[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].VolID < list[j].VolID })
	return writeRecordsLocked(list)
}

// RemoveRecord 删除一条接管记录（回滚成功后调用）。
func RemoveRecord(vgUuid string) error {
	recMu.Lock()
	defer recMu.Unlock()
	list, err := loadRecordsLocked()
	if err != nil {
		return err
	}
	out := make([]Record, 0, len(list))
	for _, r := range list {
		if r.VGUuid != vgUuid {
			out = append(out, r)
		}
	}
	return writeRecordsLocked(out)
}

// writeRecordsLocked 先写临时文件再 rename，避免半截写入损坏记录。
func writeRecordsLocked(list []Record) error {
	if err := os.MkdirAll(StateDir, 0o700); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := recordsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, recordsPath())
}

// Stamp 返回统一格式的时间戳。
func Stamp() string { return time.Now().Format("2006-01-02 15:04:05") }
