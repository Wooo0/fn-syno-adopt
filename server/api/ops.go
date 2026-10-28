// 写操作 API：接管 / 所有权修复 / @eaDir 清理 / 任务查询。
// 全部写操作走 jobs 异步任务模型，支持 DRYRUN 演练与显式确认。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"fnos-syno-adopt/adopt"
	"fnos-syno-adopt/commander"
	"fnos-syno-adopt/eadir"
	"fnos-syno-adopt/jobs"
	"fnos-syno-adopt/owner"
	"fnos-syno-adopt/vols"
)

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, 400, "请求体 JSON 解析失败: "+err.Error())
		return false
	}
	return true
}

// startJob 统一启动异步写任务。
//
// 关键点：goroutine 内 recover + Finish。若任务函数 panic 而无人收尾，
// jobs 的全局写锁（live）永远不释放，之后所有写操作都会被永久拒绝，
// 只能重启应用。panic 兜底把 panic 转成一条 error 任务日志，锁必定归还。
// Finish 本身幂等，正常收尾与 panic 兜底不会重复减锁。
func startJob(kind string, fn func(*jobs.Recorder) error) (*jobs.Job, error) {
	job, err := jobs.New(kind)
	if err != nil {
		return nil, err
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				job.Finish(fmt.Errorf("任务内部错误（已终止，写锁已释放）: %v", r))
			}
		}()
		job.Finish(fn(jobs.NewRecorder(job)))
	}()
	return job, nil
}

// uuidRE 卷组 UUID 格式校验（用户传入值一律视为不可信）。
var uuidRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ---------- 逻辑卷激活（激活未挂载的候选盘，供检测读取 fs 类型） ----------

func lvmActivateHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VGUuid string `json:"vgUuid"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.VGUuid == "" {
		fail(w, 400, "缺少 vgUuid")
		return
	}
	// argv 传参已杜绝 shell 注入，但仍校验形态：避免把任意串当作 vgchange 参数
	if !uuidRE.MatchString(req.VGUuid) {
		fail(w, 400, "vgUuid 格式非法（需 8-64 位字母数字/下划线/连字符）")
		return
	}
	res, err := commander.Run(60*time.Second, "vgchange", "-ay", req.VGUuid)
	if err != nil {
		fail(w, 500, "vgchange 失败: "+res.Stderr)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "output": res.Stdout})
}

// ---------- 接管 ----------

func adoptPlanHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VGUuids []string `json:"vgUuids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.VGUuids) == 0 {
		fail(w, 400, "未选择任何候选盘")
		return
	}
	plans := []*adopt.Plan{}
	errs := []map[string]string{}
	for _, u := range req.VGUuids {
		p, err := adopt.BuildPlan(u)
		if err != nil {
			errs = append(errs, map[string]string{"vgUuid": u, "error": err.Error()})
			continue
		}
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		fail(w, 400, "全部候选盘计划生成失败: "+errs[0]["error"])
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "plans": plans, "errors": errs,
	})
}

func adoptExecHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VGUuid  string `json:"vgUuid"`
		DryRun  bool   `json:"dryRun"`
		Confirm string `json:"confirm"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !req.DryRun && req.Confirm != "ADOPT" {
		fail(w, 400, "正式执行需要输入确认文本 ADOPT")
		return
	}
	plan, err := adopt.BuildPlan(req.VGUuid)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	job, err := startJob("adopt", func(rec *jobs.Recorder) error {
		return adopt.Execute(plan, req.DryRun, rec)
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jobId": job.ID})
}

// ---------- 所有权修复 ----------

func ownerScanHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := owner.Scan(req.Path)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "data": res})
}

func ownerRepairHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path      string `json:"path"`
		UID       int    `json:"uid"`
		GID       int    `json:"gid"`
		RemoveAcl bool   `json:"removeAcl"`
		DryRun    bool   `json:"dryRun"`
		Confirm   string `json:"confirm"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !req.DryRun && req.Confirm != "REPAIR" {
		fail(w, 400, "正式执行需要输入确认文本 REPAIR")
		return
	}
	job, err := startJob("owner-repair", func(rec *jobs.Recorder) error {
		return owner.Repair(req.Path, req.UID, req.GID, req.RemoveAcl, req.DryRun, rec)
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jobId": job.ID})
}

// ---------- @eaDir 清理 ----------

func eadirScanHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := eadir.Scan(req.Path)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "data": res})
}

func eadirCleanHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		DryRun  bool   `json:"dryRun"`
		Confirm string `json:"confirm"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !req.DryRun && req.Confirm != "CLEAN" {
		fail(w, 400, "正式执行需要输入确认文本 CLEAN")
		return
	}
	job, err := startJob("eadir-clean", func(rec *jobs.Recorder) error {
		return eadir.Clean(req.Path, req.DryRun, rec)
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jobId": job.ID})
}

// ---------- 已接管卷 / 回滚 ----------

func adoptedListHandler(w http.ResponseWriter, _ *http.Request) {
	list, err := adopt.ListAdopted()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "volumes": list})
}

func rollbackHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VGUuid  string `json:"vgUuid"`
		DryRun  bool   `json:"dryRun"`
		Confirm string `json:"confirm"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.VGUuid == "" {
		fail(w, 400, "缺少 vgUuid")
		return
	}
	if !uuidRE.MatchString(req.VGUuid) {
		fail(w, 400, "vgUuid 格式非法")
		return
	}
	if !req.DryRun && req.Confirm != "ROLLBACK" {
		fail(w, 400, "正式回滚需要输入确认文本 ROLLBACK")
		return
	}
	job, err := startJob("rollback", func(rec *jobs.Recorder) error {
		return adopt.Rollback(req.VGUuid, req.DryRun, rec)
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jobId": job.ID})
}

// ---------- 可选存储空间（清理工具下拉用） ----------

func volumesHandler(w http.ResponseWriter, _ *http.Request) {
	list, err := vols.List()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"volumes":   list,
		"recommend": vols.Recommend(list),
	})
}

// ---------- 任务查询 ----------

func jobsListHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "jobs": jobs.All()})
}

func jobGetHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	j, ok := jobs.Get(id)
	if !ok {
		fail(w, 404, "任务不存在: "+id)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "job": j})
}

// ---------- md 阵列（群晖 1.2 超级块不会内核自动组装，需用户触发） ----------

func mdScanHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "arrays": commander.MdUnassembled()})
}

func mdAssembleHandler(w http.ResponseWriter, _ *http.Request) {
	out, err := commander.MdAssemble()
	if err != nil {
		fail(w, 500, "mdadm --assemble --scan 失败: "+out)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "output": out})
}

// ---------- 系统辅助 ----------

func sysUsersHandler(w http.ResponseWriter, _ *http.Request) {
	u := owner.Suggest()
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"suggest":   u,
		"crcLoaded": adopt.CrcLoaded(),
	})
}

// RegisterOps 挂载写操作与任务查询路由。
func RegisterOps(mux *http.ServeMux) {
	mux.HandleFunc("/api/lvm/activate", lvmActivateHandler)
	mux.HandleFunc("/api/adopt/plan", adoptPlanHandler)
	mux.HandleFunc("/api/adopt/exec", adoptExecHandler)
	mux.HandleFunc("/api/adopted", adoptedListHandler)
	mux.HandleFunc("/api/adopt/rollback", rollbackHandler)
	mux.HandleFunc("/api/owner/scan", ownerScanHandler)
	mux.HandleFunc("/api/owner/repair", ownerRepairHandler)
	mux.HandleFunc("/api/eadir/scan", eadirScanHandler)
	mux.HandleFunc("/api/eadir/clean", eadirCleanHandler)
	mux.HandleFunc("/api/md/scan", mdScanHandler)
	mux.HandleFunc("/api/md/assemble", mdAssembleHandler)
	mux.HandleFunc("/api/jobs", jobsListHandler)
	mux.HandleFunc("/api/jobs/", jobGetHandler)
	mux.HandleFunc("/api/sysusers", sysUsersHandler)
	mux.HandleFunc("/api/volumes", volumesHandler)
}
