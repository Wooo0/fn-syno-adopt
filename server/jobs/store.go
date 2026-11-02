// 任务日志持久化：每个任务终态时落盘为 <storeDir>/<id>.json。
//
// 背景（真机教训 2026-10-30）：任务记录此前纯内存，页面一刷新 jobId 就丢、
// 应用一重启日志全灭——用户遇到"接管时报了个错，还没看清就消失了"，事后
// 无法回看，只能对着已完成的接管猜哪步出的错。落盘后前端「任务历史」可查。
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// storeDir 任务日志目录。与接管记录同根（/var/lib/fn-syno-adopt）。
var storeDir = "/var/lib/fn-syno-adopt/jobs"

// SetStoreDir 覆盖日志目录（main 启动时按部署环境调用；默认 /var/lib/fn-syno-adopt/jobs）。
func SetStoreDir(dir string) {
	mu.Lock()
	defer mu.Unlock()
	storeDir = dir
}

func storePath(id string) string { return filepath.Join(storeDir, id+".json") }

// saveLocked 把任务写到磁盘（调用方须持锁）。终态任务体量小（几十步），
// 同步写足够；临时文件 + rename 原子落盘，坏一半也不损旧文件。
func saveLocked(j *Job) {
	if storeDir == "" {
		return
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		fmt.Println("[jobs] 创建日志目录失败:", err)
		return
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		fmt.Println("[jobs] 序列化任务失败:", err)
		return
	}
	tmp := storePath(j.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		fmt.Println("[jobs] 写任务日志失败:", err)
		return
	}
	if err := os.Rename(tmp, storePath(j.ID)); err != nil {
		fmt.Println("[jobs] 原子替换任务日志失败:", err)
		_ = os.Remove(tmp)
	}
}

// LoadStored 启动时把磁盘上的历史任务载入内存（应用重启后仍可回看）。
// 单个文件损坏只跳过该文件，不阻断整体。
func LoadStored() {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return // 目录不存在 = 从无历史，正常
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(storeDir, name))
		if err != nil {
			continue
		}
		var j Job
		if err := json.Unmarshal(b, &j); err != nil || j.ID == "" {
			continue // 损坏文件跳过
		}
		if _, dup := all[j.ID]; !dup {
			all[j.ID] = &j
		}
	}
}

// RemoveStored 删除一条历史任务（磁盘 + 内存）。
func RemoveStored(id string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := all[id]; !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	delete(all, id)
	if err := os.Remove(storePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearStored 清空全部历史任务。
func ClearStored() error {
	mu.Lock()
	defer mu.Unlock()
	entries, err := os.ReadDir(storeDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
				_ = os.Remove(filepath.Join(storeDir, e.Name()))
			}
		}
	}
	all = map[string]*Job{}
	return nil
}

// StoredIDs 返回磁盘上的历史任务 ID（测试/诊断用）。
func StoredIDs() []string {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}
