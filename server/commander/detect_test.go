package commander

import "testing"

// GLM 的验收标准之一：degraded / recovering / resyncing 的阵列必须被拦截。
// 这里固化判定规则（纯函数，不依赖 mdadm）。
func TestMdHealthy(t *testing.T) {
	cases := []struct {
		name string
		d    MdDetail
		want bool
	}{
		{"clean", MdDetail{State: "clean", Members: 4, Active: 4}, true},
		{"clean raid5 8盘", MdDetail{State: "clean", Level: "raid5", Members: 8, Active: 8}, true},
		{"degraded", MdDetail{State: "clean, degraded", Members: 4, Active: 3}, false},
		{"recovering", MdDetail{State: "clean, recovering", Members: 4, Active: 4}, false},
		{"resyncing", MdDetail{State: "clean, resyncing", Members: 4, Active: 4}, false},
		{"rebuilding", MdDetail{State: "clean, rebuilding", Members: 4, Active: 3}, false},
		{"有成员掉线但状态写 clean", MdDetail{State: "clean", Members: 8, Active: 7}, false},
		{"raid0 无 State 行", MdDetail{Level: "raid0", Members: 2, Active: 2}, true},
		{"空状态视为健康", MdDetail{Members: 1, Active: 1}, true},
	}
	for _, c := range cases {
		if got := MdHealthy(c.d); got != c.want {
			t.Errorf("%s: 期望 healthy=%v，实际 %v", c.name, c.want, got)
		}
	}
}

func TestIsVolPoint(t *testing.T) {
	cases := map[string]bool{
		"/vol1":       true,
		"/vol12":      true,
		"/vol1/photo": false, // 子目录不是存储空间挂载点
		"/volume1":    false,
		"/vol":        false,
		"/mnt/data":   false,
	}
	for in, want := range cases {
		if got := isVolPoint(in); got != want {
			t.Errorf("isVolPoint(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestStripPartition(t *testing.T) {
	cases := map[string]string{
		"/dev/sdd3":       "/dev/sdd",
		"/dev/sda":        "/dev/sda",
		"/dev/nvme0n1p3":  "/dev/nvme0n1",
		"/dev/mmcblk0p2":  "/dev/mmcblk0",
		"/dev/mapper/foo": "/dev/mapper/foo",
	}
	for in, want := range cases {
		if got := stripPartition(in); got != want {
			t.Errorf("stripPartition(%q) = %q，期望 %q", in, got, want)
		}
	}
}
