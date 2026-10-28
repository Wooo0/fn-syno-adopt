package vols

import (
	"strings"
	"testing"

	"fnos-syno-adopt/adopt"
)

// Recommend 的取值顺序是产品约定：本应用接管的群晖盘 > 非系统卷 > 第一个。
// 选错默认值的代价很大（用户可能对 /vol1 系统卷动 chown），固化为测试。
func TestRecommend(t *testing.T) {
	list := []Volume{
		{Path: "/vol1", IsSystem: true},
		{Path: "/vol2"},
		{Path: "/vol3", Adopted: true},
	}
	if got := Recommend(list); got != "/vol3" {
		t.Errorf("有接管盘时应推荐接管盘，实际 %q", got)
	}

	noAdopted := []Volume{{Path: "/vol1", IsSystem: true}, {Path: "/vol2"}}
	if got := Recommend(noAdopted); got != "/vol2" {
		t.Errorf("无接管盘时应跳过系统卷，实际 %q", got)
	}

	onlySystem := []Volume{{Path: "/vol1", IsSystem: true}}
	if got := Recommend(onlySystem); got != "/vol1" {
		t.Errorf("只有系统卷时回落第一个，实际 %q", got)
	}
	if got := Recommend(nil); got != "" {
		t.Errorf("空列表应返回空串，实际 %q", got)
	}
}

func TestHuman(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		512:     "512 B",
		2048:    "2.0 KB",
		1536:    "1.5 KB",
		5 << 30: "5.0 GB",
	}
	for in, want := range cases {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %q，期望 %q", in, got, want)
		}
	}
}

// 挂载点按数字排序，不能出现 /vol10 排在 /vol2 前面。
func TestVolNumOrder(t *testing.T) {
	if volNum("/vol2") >= volNum("/vol10") {
		t.Errorf("/vol2 应排在 /vol10 之前")
	}
	if !strings.HasPrefix("/vol3", "/vol") {
		t.Fatal("测试数据异常")
	}
	// adopt 包被引用以确保编译期依赖正确（列表需要接管记录做标记）
	_ = adopt.Record{}
}
