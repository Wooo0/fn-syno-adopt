package commander

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 真机 2026-10-31 事故回归：解析归档时把 physical_volumes 区块名当成了卷组名，
// 回滚直接执行了 vgrename <uuid> physical_volumes，把一块好盘的卷组改名成
// 该名字，检测面板随即认不出它。这段测试把当时的真实归档结构钉死。
const synoArchiveSample = `vg1002 {
	id = "AbCdEf-1234-GhIj-KlMn-OpQrStUvWxYz"
	seqno = 12
	format = "lvm2"
	status = ["RESIZEABLE", "READ", "WRITE"]
	extent_size = 8192
	max_lv = 0
	max_pv = 0
	metadata_copies = 0

	physical_volumes {

		pv0 {
			id = "Pv00Pv-0000-0000-0000-000000000000"
			device = "/dev/md4"
			status = ["ALLOCATABLE"]
			pe_start = 2048
			pe_count = 476931
		}
	}

	logical_volumes {

		lv {
			id = "Lv00Lv-0000-0000-0000-000000000000"
			status = ["READ", "WRITE", "VISIBLE"]
			creation_time = 1698000000
			segment_count = 1

			segment1 {
				start_extent = 0
				extent_count = 476931

				type = "striped"
				stripe_count = 1

				stripes = [
					"pv0", 0
				]
			}
		}
	}
}
`

func writeArchive(t *testing.T, dir, name, body string, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写归档失败: %v", err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatalf("改时间戳失败: %v", err)
	}
}

// 正常情况：最早一份归档里是群晖原名。
func TestOriginOfNormal(t *testing.T) {
	dir := t.TempDir()
	old := archDir
	archDir = dir
	defer func() { archDir = old }()

	writeArchive(t, dir, "vg1002_00010-1700000000.vg", synoArchiveSample,
		time.Now().Add(-2*time.Hour))

	o, ok := OriginOf("AbCdEf-1234-GhIj-KlMn-OpQrStUvWxYz")
	if !ok {
		t.Fatal("应当能从归档回溯出原始名")
	}
	if o.VG != "vg1002" {
		t.Fatalf("卷组名解析错误：想要 vg1002，实际 %q（这是那次改坏盘的同款 bug）", o.VG)
	}
	if len(o.LVs) != 1 || o.LVs[0] != "lv" {
		t.Fatalf("逻辑卷名解析错误：想要 [lv]，实际 %v", o.LVs)
	}
}

// 事故现场：最早一份归档的卷组块名已经被写成 physical_volumes，
// 后续归档里才是正确的 vg1002 —— 必须往后翻而不是直接放弃。
func TestOriginOfSkipsCorruptedArchive(t *testing.T) {
	dir := t.TempDir()
	old := archDir
	archDir = dir
	defer func() { archDir = old }()

	bad := "physical_volumes {\n\tid = \"AbCdEf-1234-GhIj-KlMn-OpQrStUvWxYz\"\n}\n"
	writeArchive(t, dir, "physical_volumes_00013-1700000500.vg", bad,
		time.Now().Add(-2*time.Hour))
	writeArchive(t, dir, "vg1002_00012-1700000100.vg", synoArchiveSample,
		time.Now().Add(-1*time.Hour))

	o, ok := OriginOf("AbCdEf-1234-GhIj-KlMn-OpQrStUvWxYz")
	if !ok {
		t.Fatal("应当跳过坏归档，从下一份拿到原名")
	}
	if o.VG != "vg1002" {
		t.Fatalf("卷组名解析错误：想要 vg1002，实际 %q", o.VG)
	}
}

// 所有归档都不像群晖卷 → 必须返回 false，让调用方转人工，
// 绝不能拿着一个莫名其妙的区块名去 vgrename。
func TestOriginOfRejectsNonSynoName(t *testing.T) {
	dir := t.TempDir()
	old := archDir
	archDir = dir
	defer func() { archDir = old }()

	bad := "physical_volumes {\n\tid = \"AbCdEf-1234-GhIj-KlMn-OpQrStUvWxYz\"\n}\n"
	writeArchive(t, dir, "physical_volumes_00013-1700000500.vg", bad, time.Now())

	if o, ok := OriginOf("AbCdEf-1234-GhIj-KlMn-OpQrStUvWxYz"); ok {
		t.Fatalf("非群晖命名不应通过校验，实际返回 %+v", o)
	}
}
