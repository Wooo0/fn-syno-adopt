package adopt

import "testing"

// 本测试固化一个不可妥协的承诺：回滚移除 mdadm.conf 时，
// 只能删"本应用写过的行"，用户/系统原有的配置一个字节都不能动。
func TestFilterConfLines(t *testing.T) {
	const mine = "aaaa1111"
	const other = "bbbb2222"

	body := `# mdadm.conf 原始头部
DEVICE partitions
# fn-syno-adopt 2026-10-28 21:00:00
ARRAY /dev/md/2 metadata=1.2 UUID=` + mine + ` name=fn:2
ARRAY /dev/md/0 metadata=1.2 UUID=` + other + ` name=fn:0
# fn-syno-adopt 2026-10-28 21:10:00
ARRAY /dev/md/3 metadata=1.2 UUID=` + mine + ` name=fn:3
ARRAY /dev/md/1 metadata=1.2 UUID=` + other + ` name=fn:1
`

	got, removed := filterConfLines(body, []string{mine})

	if removed != 2 {
		t.Fatalf("应移除 2 行，实际 %d", removed)
	}
	// 别人的 ARRAY 必须原样存活
	for _, want := range []string{
		"# mdadm.conf 原始头部",
		"DEVICE partitions",
		"ARRAY /dev/md/0 metadata=1.2 UUID=" + other,
		"ARRAY /dev/md/1 metadata=1.2 UUID=" + other,
	} {
		if !contains(got, want) {
			t.Errorf("原有内容被误删: %q\n实际结果:\n%s", want, got)
		}
	}
	// 我们写的必须消失（含 UUID 与标记行）
	for _, bad := range []string{mine, "ARRAY /dev/md/2", "ARRAY /dev/md/3", confMark} {
		if contains(got, bad) {
			t.Errorf("本应用写入的内容未清除: %q\n实际结果:\n%s", bad, got)
		}
	}
}

// 别人的 ARRAY 紧跟在我们的标记行之后时，标记行与 ARRAY 行都必须保留。
func TestFilterConfLinesKeepsForeignArrayUnderOurMark(t *testing.T) {
	body := confMark + " 2026-10-28 21:00:00\nARRAY /dev/md/9 metadata=1.2 UUID=zzzz9999 name=fn:9\n"
	got, removed := filterConfLines(body, []string{"aaaa1111"})
	if removed != 0 {
		t.Fatalf("UUID 不匹配时不应删除，实际删除 %d 行", removed)
	}
	if !contains(got, "ARRAY /dev/md/9") || !contains(got, confMark) {
		t.Errorf("UUID 不匹配时标记行与 ARRAY 行都应保留，实际:\n%s", got)
	}
}

// 空 UUID（v0.5.x 老记录）不应删任何东西。
func TestFilterConfLinesNoUUID(t *testing.T) {
	body := confMark + "\nARRAY /dev/md/2 UUID=aaaa1111\n"
	got, removed := filterConfLines(body, nil)
	if removed != 0 || got != body {
		t.Errorf("无 UUID 时文件应原样保留，removed=%d\n%s", removed, got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
