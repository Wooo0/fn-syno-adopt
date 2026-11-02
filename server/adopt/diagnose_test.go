package adopt

import "testing"

// 真机 2026-10-31 的内核日志原文（sdb / vg1001-lv）。当时预检把原因错判成
// 树根的私有 root flag，给出了"只能换环境"的错误建议；实际是叶子里的 inode
// 带标（bit 30，0x40000000），可以在旧内核里重建子卷后继续接管。
// 这几行钉住解析：两处命中、root=257、ino 取得对、mirror 1/2 只算一条。
func TestScanLeafFlagsRealLog(t *testing.T) {
	logs := []string{
		"[14851.461160] BTRFS critical (device dm-4): corrupt leaf: root=257 block=173670400 slot=116 ino=3207, unknown incompat flags detected: 0x40000000",
		"[14851.461459] BTRFS error (device dm-4): read time tree block corruption detected on logical 173670400 mirror 1",
		"[14851.464772] aops:btree_aops [btrfs] ino:1",
		"[14851.464853] BTRFS critical (device dm-4): corrupt leaf: root=257 block=173670400 slot=116 ino=3207, unknown incompat flags detected: 0x40000000",
		"[14851.465182] BTRFS error (device dm-4): read time tree block corruption detected on logical 173670400 mirror 2",
		"[14851.465555] BTRFS info (device dm-4): last unmount of filesystem fa13fc10",
	}
	hits := scanLeafFlags(logs)
	if len(hits) != 2 {
		t.Fatalf("应当命中 2 条（mirror 1 / mirror 2 各一条），实际 %d", len(hits))
	}
	if hits[0].Root != 257 {
		t.Fatalf("root 解析错误：想要 257，实际 %d", hits[0].Root)
	}
	if hits[0].Ino != 3207 {
		t.Fatalf("ino 解析错误：想要 3207，实际 %d", hits[0].Ino)
	}
	if hits[0].Slot != 116 {
		t.Fatalf("slot 解析错误：想要 116，实际 %d", hits[0].Slot)
	}
	if hits[0].Flag != "0x40000000" {
		t.Fatalf("标志解析错误：想要 0x40000000，实际 %s", hits[0].Flag)
	}
}

// 树根级的私有 root flag（另一类问题）不应该被当成 inode 标志命中。
func TestScanLeafFlagsIgnoresRootFlag(t *testing.T) {
	logs := []string{
		"[  123.456] BTRFS error (device dm-1): invalid root flags, have 0x400000000 expect mask 0x1000000000001",
	}
	if hits := scanLeafFlags(logs); len(hits) != 0 {
		t.Fatalf("root flag 不应命中 inode 标志解析，实际 %+v", hits)
	}
	if !rootFlagRE.MatchString(logs[0]) {
		t.Fatal("root flag 正则应当能识别这一行")
	}
}
