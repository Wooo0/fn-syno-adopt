// Package jobs 提供极简的异步任务管理：
// 写操作（接管/所有权修复/@eaDir 清理）耗时可能长达数分钟到数小时，
// 统一走"提交任务 → 轮询日志"模型，避免 HTTP 请求被网关超时掐断。
package jobs

import (
	"fmt"
	"sync"
	"time"
)

// StepLog 任务中的一步执行记录。
type StepLog struct {
	Name    string `json:"name"`    // 步骤名
	Command string `json:"command"` // 展示用命令行
	Status  string `json:"status"`  // ok / fail / skip / pending
	Detail  string `json:"detail"`  // 输出摘要或错误信息
}

// Job 一次异步执行。
// Steps 用 []*StepLog（而非 []StepLog）：AddStep 返回元素指针供调用方后续
// Done() 落终态；值切片在 append 扩容后会整体搬移，旧指针会指向已脱离的
// 底层数组，导致步骤永远停在 pending。指针切片杜绝该隐患。
type Job struct {
	ID     string     `json:"id"`
	Kind   string     `json:"kind"`   // adopt / owner-repair / eadir-clean / rollback
	Status string     `json:"status"` // running / done / error
	Error  string     `json:"error,omitempty"`
	Steps  []*StepLog `json:"steps"`
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end,omitempty"`
}

var (
	mu   sync.Mutex
	all  = map[string]*Job{}
	seq  = 0
	live = 0 // 同时只允许一个写任务在跑
)

// New 登记一个新任务。同一时刻只允许一个运行中的写任务（系统级互斥）。
func New(kind string) (*Job, error) {
	mu.Lock()
	defer mu.Unlock()
	if live > 0 {
		return nil, fmt.Errorf("已有写操作任务在执行中，请等待其完成后再试")
	}
	seq++
	j := &Job{
		ID:    fmt.Sprintf("job-%d-%d", time.Now().Unix(), seq),
		Kind:  kind,
		Steps: []*StepLog{},
		Start: time.Now(),
	}
	all[j.ID] = j
	live++
	return j, nil
}

// AddStep 追加一条步骤记录（pending 起步，随后改为终态）。
// 返回指针：切片扩容只搬移指针而非结构体，指针始终有效。
func (j *Job) AddStep(name, command string) *StepLog {
	mu.Lock()
	defer mu.Unlock()
	s := &StepLog{Name: name, Command: command, Status: "pending"}
	j.Steps = append(j.Steps, s)
	return s
}

// Finish 收尾任务；err 非 nil 时标记 error。
func (j *Job) Finish(err error) {
	mu.Lock()
	defer mu.Unlock()
	if j.Status == "done" || j.Status == "error" {
		return // 幂等：panic 兜底与正常收尾只生效一次，避免 live 被多减
	}
	now := time.Now()
	j.End = &now
	if err != nil {
		j.Status = "error"
		j.Error = err.Error()
	} else {
		j.Status = "done"
	}
	live--
	saveLocked(j) // 终态落盘：应用重启/页面刷新后仍可在「任务历史」回看
}

// Get 按 ID 取任务。
func Get(id string) (*Job, bool) {
	mu.Lock()
	defer mu.Unlock()
	j, ok := all[id]
	return j, ok
}

// All 返回全部任务（按创建时间倒序，最新的在前）。
func All() []*Job {
	mu.Lock()
	defer mu.Unlock()
	list := make([]*Job, 0, len(all))
	for _, j := range all {
		list = append(list, j)
	}
	// 倒序：ID 内含单调递增 seq，直接按 ID 反排
	for i := 0; i < len(list); i++ {
		for k := i + 1; k < len(list); k++ {
			if list[k].ID > list[i].ID {
				list[i], list[k] = list[k], list[i]
			}
		}
	}
	return list
}

// StepHandle 单个步骤的句柄：先 Add 挂起，完成后 Done 落终态。
type StepHandle interface {
	Done(status, detail string)
}

// Recorder 顺序步骤记录器，供各写操作包持有（并发安全由 Job 内锁保证）。
type Recorder struct{ j *Job }

// NewRecorder 包装任务为记录器。
func NewRecorder(j *Job) *Recorder { return &Recorder{j} }

// Add 追加一条 pending 步骤。
func (r *Recorder) Add(name, command string) StepHandle {
	return &stepHandle{r.j.AddStep(name, command)}
}

type stepHandle struct{ sl *StepLog }

func (h *stepHandle) Done(status, detail string) {
	h.sl.Status = status
	h.sl.Detail = detail
}
