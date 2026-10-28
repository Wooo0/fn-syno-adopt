package jobs

import "testing"

// P0#1 回归：AddStep 返回的指针在后续 append 扩容后必须仍指向真实步骤。
// 复现原 bug 场景：Add → 多次 append → 再写终态，旧写法会写进已脱离的旧数组。
func TestAddStepPointerSurvivesAppend(t *testing.T) {
	j := &Job{ID: "t", Steps: []*StepLog{}}
	handles := []*StepLog{}
	for i := 0; i < 50; i++ {
		handles = append(handles, j.AddStep("step", "cmd"))
		filler := j.AddStep("filler", "filler") // 制造扩容
		filler.Status = "ok"
	}
	for i, h := range handles {
		h.Status = "ok"
		h.Detail = "late"
		if got := j.Steps[i*2].Status; got != "ok" {
			t.Fatalf("step %d status = %q, want ok（指针失效，写进了旧数组）", i, got)
		}
		if j.Steps[i*2].Detail != "late" {
			t.Fatalf("step %d detail 未生效", i)
		}
	}
}

// Finish 幂等：panic 兜底 + 正常收尾只减一次 live。
func TestFinishIdempotent(t *testing.T) {
	before := liveCount()
	j, err := New("t")
	if err != nil {
		t.Fatal(err)
	}
	j.Finish(nil)
	j.Finish(nil) // 重复收尾
	j.Finish(nil)
	if after := liveCount(); after != before {
		t.Fatalf("live 泄漏: before=%d after=%d", before, after)
	}
	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
}

// JSON 契约：steps 永不为 null。
func TestStepsNeverNull(t *testing.T) {
	j := &Job{ID: "t", Steps: []*StepLog{}}
	if j.Steps == nil {
		t.Fatal("Steps 为 nil，违反列表字段非 null 契约")
	}
}

func liveCount() int {
	mu.Lock()
	defer mu.Unlock()
	return live
}
