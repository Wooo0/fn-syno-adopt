package commander

import (
	"os"
	"testing"
)

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

// 真机实测（用户 8 块群晖盘）得出的命名规则，早期版本只认 vg1/volume_1 导致全部落空。
func TestIsSynoDataVolume(t *testing.T) {
	yes := [][2]string{
		{"vg1", "volume_7"}, {"vg2", "volume_6"}, {"vg3", "volume_8"},
		{"vg1", "volume_1"},                // 老式基线
		{"vg1001", "lv"}, {"vg1002", "lv"}, // DSM 7 新式
	}
	no := [][2]string{
		{"vg1", "syno_vg_reserved_area"}, // 12M 保留区，不是数据卷
		{"trim_abc123", "0"},             // 飞牛原生/已接管
		{"vg1", "volume"},                // 缺编号
		{"vgx", "volume_1"},              // VG 名不符
		{"vg1", "syno_vg_reserved_area_2"},
	}
	for _, c := range yes {
		if !IsSynoDataVolume(c[0], c[1]) {
			t.Errorf("应识别为群晖数据卷: %s/%s", c[0], c[1])
		}
	}
	for _, c := range no {
		if IsSynoDataVolume(c[0], c[1]) {
			t.Errorf("不应识别为数据卷: %s/%s", c[0], c[1])
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

// TestResolveBlockDev 固化"卷组未激活时设备节点不可用"的判定。
// 真机坑：lvs 给的 /dev/mapper/vgN-lv 在未激活时不是可用块设备，
// 直接喂给 btrfs 会报 "not a directory / can't access"。
func TestResolveBlockDev(t *testing.T) {
	// 一个肯定不存在的路径：必须判定为不可用并给出原因
	if p, why := ResolveBlockDev("/dev/mapper/definitely-not-exist-0", "vg1", "lv"); p != "" || why == "" {
		t.Errorf("不存在的设备应返回空路径+原因，实际 p=%q why=%q", p, why)
	}
	// 目录不是块设备：必须拒绝（否则 btrfs 会拿它当挂载点，读到的是系统盘）
	if p, _ := ResolveBlockDev(t.TempDir(), "vg1", "lv"); p != "" {
		t.Errorf("目录不应被当作块设备，实际返回 %q", p)
	}
	// 字符设备（/dev/null）也不是块设备
	if p, _ := ResolveBlockDev("/dev/null", "vg1", "lv"); p != "" {
		t.Errorf("/dev/null 是字符设备，不应通过，实际返回 %q", p)
	}
	// 真实块设备应原样返回（/dev/loop0 或无该设备时跳过，保证测试在 CI 也能过）
	if b, err := os.Stat("/dev/loop0"); err == nil && isBlockDevMode(b.Mode()) {
		if p, _ := ResolveBlockDev("/dev/loop0", "vg1", "lv"); p != "/dev/loop0" {
			t.Errorf("真实块设备应原样返回，实际 %q", p)
		}
	}
}

func TestIsBlockDevMode(t *testing.T) {
	dir, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if isBlockDevMode(dir.Mode()) {
		t.Error("目录不应判定为块设备")
	}
	if nul, err := os.Stat("/dev/null"); err == nil && isBlockDevMode(nul.Mode()) {
		t.Error("/dev/null 是字符设备，不应判定为块设备")
	}
}
