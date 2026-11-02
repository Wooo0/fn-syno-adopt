#!/usr/bin/env python3
# 列出 btrfs 子卷里带私有 inode 标记的文件（纯用户态，只读）。
#
# 原理：btrfs inspect-internal dump-tree 会把 fs tree 的全部元数据项打印成文本，
# 其中 INODE_ITEM 带每个文件的 flags，DIR_ITEM/INODE_REF 带目录结构。
# 本脚本解析这份文本，重建每个被标 inode 的完整路径。
#
# 用法（在飞牛上，对准要查的子卷）：
#   sudo btrfs inspect-internal dump-tree -t 257 /dev/md2 | python3 list-flagged-files.py > /tmp/md2-flagged.txt
#   sudo btrfs inspect-internal dump-tree -t 259 /dev/md3 | python3 list-flagged-files.py > /tmp/md3-flagged.txt
#   sudo btrfs inspect-internal dump-tree -t 258 /dev/mapper/vg1-volume_7 | python3 list-flagged-files.py > /tmp/sda-flagged.txt
#
# -t 后面的数字 = 子卷 ID（subvolume list 里看到的那个，如 257）。
# 主线内核合法 inode flags 只有低 12 位（0xFFF），高位带 0x48193300 的都是 DSM 私有标记。
import re
import sys

# stdout 可能接中文路径，且重定向时编码取决于 locale，统一容错
try:
    sys.stdout.reconfigure(errors='replace')
except Exception:
    pass

PRIVATE_MASK = ~0xFFF  # 低 12 位是主线合法 inode flags，其余都是私有位

item_re = re.compile(r'key \((\d+) (INODE_ITEM|INODE_REF|DIR_ITEM|DIR_INDEX|ORPHAN_ITEM|EXTENT_DATA|XATTR_ITEM|FILE_EXTENT) (\d+)\) itemoff')
flags_re = re.compile(r'flags 0x([0-9a-fA-F]+)')
loc_re = re.compile(r'location key \((\d+) INODE_ITEM')
name_re = re.compile(r'name: (.*)$')

flags = {}          # inode -> int
parents = {}        # child inode -> [(parent inode, name), ...]

cur_type = None
cur_ino = 0
cur_parent = 0
cur_loc_child = None
cur_name = None

def flush_diritem():
    global cur_loc_child, cur_name
    if cur_type in ('DIR_ITEM', 'DIR_INDEX') and cur_loc_child is not None and cur_name is not None:
        parents.setdefault(cur_loc_child, []).append((cur_ino, cur_name))
    if cur_type == 'INODE_REF' and cur_name is not None:
        # key=(子inode INODE_REF 父inode)，name 是子 inode 在父目录里的名字
        parents.setdefault(cur_ino, []).append((cur_parent, cur_name))
    cur_loc_child = None
    cur_name = None

for raw in sys.stdin.buffer:
    # dump-tree 输出里文件名可能是任意字节（旧中文 GBK 名等），逐行容错解码
    line = raw.decode('utf-8', errors='replace')
    m = item_re.search(line)
    if m:
        flush_diritem()
        cur_ino, cur_type, cur_parent = int(m.group(1)), m.group(2), int(m.group(3))
        continue
    if cur_type is None:
        continue
    if cur_type == 'INODE_ITEM':
        fm = flags_re.search(line)
        if fm and cur_ino not in flags:
            flags[cur_ino] = int(fm.group(1), 16)
    elif cur_type in ('DIR_ITEM', 'DIR_INDEX'):
        lm = loc_re.search(line)
        if lm:
            cur_loc_child = int(lm.group(1))
        nm = name_re.search(line)
        if nm:
            cur_name = nm.group(1).strip()
    elif cur_type == 'INODE_REF':
        nm = name_re.search(line)
        if nm:
            cur_name = nm.group(1).strip()
flush_diritem()

def path_of(ino):
    parts = []
    seen = set()
    while ino not in (256, 0) and ino not in seen:  # 256 = 子卷根目录
        seen.add(ino)
        pl = parents.get(ino)
        if not pl:
            parts.append('<ino %d>' % ino)
            break
        p, name = pl[0]
        parts.append(name)
        ino = p
    return '/' + '/'.join(reversed(parts))

bad = {ino: f for ino, f in flags.items() if f & PRIVATE_MASK}
print('== 总 inode: %d，带私有标记: %d ==' % (len(flags), len(bad)))
if not bad:
    print('（该子卷 inode 层干净）')
    sys.exit(0)
print('== 被标文件清单（flags 路径）==')
for ino, f in sorted(bad.items(), key=lambda x: path_of(x[0])):
    print('0x%08x  %s' % (f, path_of(ino)))
