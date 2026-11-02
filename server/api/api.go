// Package api 提供检测阶段的只读 HTTP API。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"fnos-syno-adopt/adopt"
	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/jobs"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// Version 应用版本，build.sh 从 manifest 读取后经 -ldflags -X 注入。
// 唯一可信来源是 manifest；前端侧栏显示它（此前前端硬编码 v0.5.5，
// 与 manifest 脱节，面板版本显示失去意义）。
var Version = "dev"

type SystemInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	DryRun   bool   `json:"dryRun"`
	Version  string `json:"version"`
}

func systemHandler(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"data": SystemInfo{
			Hostname: host,
			OS:       readOSName(),
			Kernel:   commander.KernelVersion(),
			DryRun:   commander.IsDryRun(),
			Version:  Version,
		},
	})
}

func readOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}

// recKey 生成一条接管记录的稳定标识：direct 认 md 阵列设备，LVM 认卷组 UUID
// （vgrename 不改 UUID，所以改名前后都能对上）。
func recKey(r adopt.Record) string {
	if r.MdDev != "" {
		return "md:" + r.MdDev
	}
	return "vg:" + r.VGUuid
}

func disksHandler(w http.ResponseWriter, _ *http.Request) {
	candidates, disks, err := commander.DetectSynologyDisks()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// 已由本工具接管的盘打标并取消"可接管"：接管是终态，重复接管没有意义，
	// 还可能对已挂载的 /volN 再生成一套冲突计划（真机实测：md2 转正后仍显示可接管）。
	// 判定依据是接管记录（direct 认 MdDev，LVM 认 VGUuid——vgrename 不改 UUID）。
	if recs, rerr := adopt.LoadRecords(); rerr == nil {
		takenMd, takenVG := map[string]adopt.Record{}, map[string]adopt.Record{}
		for _, r := range recs {
			if r.MdDev != "" {
				takenMd[r.MdDev] = r
			}
			if r.VGUuid != "" {
				takenVG[r.VGUuid] = r
			}
		}
		// matched 的键必须是**记录标识**，不能用索引：下面两个循环的 i 一个是
		// candidate 下标、一个是 recs 下标，两者毫无关系。真机 2026-10-31 就是
		// 因为拿 candidate 下标写、拿 recs 下标读，导致 /vol5 那条记录被误判成
		// "已有候选打标过"，合成展示行被跳过——检测面板比实际少了一块盘。
		matched := map[string]bool{}
		for i := range candidates {
			c := &candidates[i]
			var rec *adopt.Record
			if c.MdDev != "" {
				if r, ok := takenMd[c.MdDev]; ok {
					rec = &r
				}
			}
			if rec == nil && c.VGUuid != "" {
				if r, ok := takenVG[c.VGUuid]; ok {
					rec = &r
				}
			}
			if rec == nil {
				continue
			}
			matched[recKey(*rec)] = true
			c.Adopted = true
			c.Adoptable = false
			c.Notes = append(c.Notes, fmt.Sprintf(
				"✔ 已由本工具接管（挂载点 %s）——接管是终态，无需重复操作；"+
					"要还原请用「已接管卷 · 回滚」", rec.MountPoint))
		}
		// LVM 接管盘改名 trim_xxx 后被检测逻辑跳过（防误操作的设计），
		// 但"彻底隐身"让用户以为盘丢了（真机实测：sde 接管成功后列表里找不到）。
		// 对没匹配到候选的记录合成一条"已接管"展示行——只读信息，不影响任何操作。
		for _, r := range recs {
			if matched[recKey(r)] || r.MdDev != "" { // direct 盘不经过 LVM 循环，永远有候选可打标
				continue
			}
			dev := "/dev/mapper/" + r.NewVG + "-0"
			_, statErr := os.Stat(dev)
			// 已接管行的子卷：以前硬编码空数组，界面上表现为"接管过的盘子卷消失了"，
			// 极易被误读成数据丢失。改为复用现有挂载点读一次（不重复挂载，避免 EBUSY）；
			// 读不到就把原因写进 notes——列表为空是"没读到"，不是"没有"。
			subs := []string{}
			subWhy := ""
			for _, mp := range append([]string{r.MountPoint}, commander.MountPointsOf(dev)...) {
				if mp == "" {
					continue
				}
				list, e := commander.BtrfsSubvolIDsErr(mp)
				if e != nil {
					subWhy = e.Error()
					continue
				}
				for _, sv := range list {
					subs = append(subs, sv.Path)
				}
				subWhy = ""
				break
			}
			notes := []string{fmt.Sprintf(
				"✔ 已由本工具接管（原 %s/%s → 挂载点 %s）——接管后卷组已改名，"+
					"接管是终态，无需重复操作；要还原请用「已接管卷 · 回滚」",
				r.OldVG, r.OldLV, r.MountPoint)}
			if len(subs) == 0 {
				msg := "已接管行未列出子卷（该卷当前未挂载时不读子卷），不影响数据与回滚"
				if subWhy != "" {
					msg = "已接管行未列出子卷：" + subWhy + "（不影响数据与回滚）"
				}
				notes = append(notes, msg)
			}
			// 盘符按当前实际设备反查，不用记录里的旧值：盘符会漂移，
			// 真机 2026-10-31 该卷接管时是 sde、现在实际是 sdd，照抄记录就会
			// 出现两行同名 sde、真正的 sdd 看起来"消失"。
			curDisk := commander.DiskOfVG(r.NewVG)
			if curDisk == "" {
				curDisk = r.Disk // 反查不到（卷未激活等）再退回记录值
			}
			candidates = append(candidates, commander.SynoCandidate{
				VG: r.NewVG, VGUuid: r.VGUuid, LV: "0",
				DMPath: dev, LVSize: r.LVSize, Disk: curDisk,
				LVActive: statErr == nil, DevReady: statErr == nil,
				FS: "btrfs", Subvols: subs,
				Adopted: true, Adoptable: false,
				Kind:      "basic",
				PhysDisks: []string{},
				Notes:     notes,
			})
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"candidates": candidates,
		"disks":      disks,
	})
}

// jobDeleteHandler 删除一条历史任务（挂 /api/jobs-delete/ 前缀，
// 避开 ops.go 已注册的 /api/jobs/ 详情路由——ServeMux 重复注册会 panic）。
func jobDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/jobs-delete/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, 400, "缺少任务 ID")
		return
	}
	if err := jobs.RemoveStored(id); err != nil {
		fail(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// Register 挂载全部 API 路由（只读检测 + 写操作）。
func Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/system", systemHandler)
	mux.HandleFunc("/api/disks", disksHandler)
	mux.HandleFunc("/api/jobs-delete/", jobDeleteHandler)
	RegisterOps(mux)
}
