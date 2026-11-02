#!/bin/bash
# 只读摸底：统计群晖 DSM7 私有标记在盘上的分布，不改盘一个字节。
#
# 两类私有 flag，位置、报错、处置完全不同（真机 2026-10-31 各撞一次）：
#   ① root item 的 root flags：0x400000000（bit34）
#      → open_ctree 就拒，连子卷列表都列不出来
#      → 内核日志：invalid root flags, have 0x400000000 expect mask ...
#   ② inode item 的 inode flags：0x40000000（bit30）
#      → 根能挂、子卷列表能列，但该子卷一读就判 corrupt leaf，整个子卷挂不上
#      → 内核日志：corrupt leaf: root=257 ... ino=8512, unknown incompat flags detected
#
# 用法：
#   sudo bash inspect-syno-flags.sh /dev/mapper/vg1001-lv
#   sudo bash inspect-syno-flags.sh /dev/mapper/vg1001-lv 257 200 201 202
#
# 说明：
#   - dump-tree 是纯用户态元数据解析，不写盘、不受内核 tree-checker 影响。
#   - fs tree 全量文本 dump 可能很大，走管道流式统计，不落盘。
#
# 修订（2026-10-31）：
#   - 修掉 `for v in $(grep -oE 'flags 0x..')` 的按空格分词 bug（会把 "flags" 和
#     "0x0" 拆成两个条目，并给标准值 0x0/0x1 打出"非标准"的吓人结论）
#   - fs tree 统计排除 `leaf ... flags 0x1(WRITTEN)` 这类叶子头行，只数 inode item
#   - 增加子卷 ID → 目录名映射，和末尾判定

set -u
DEV=${1:?用法: sudo bash $0 /dev/mapper/vgXXX-lv [fs_tree_id ...]}
[ -e "$DEV" ] || { echo "设备不存在: $DEV"; exit 1; }
command -v btrfs >/dev/null || { echo "缺 btrfs-progs"; exit 1; }

PRIVATE_ROOT_BIT=$((0x400000000))   # bit34
PRIVATE_INODE_BIT=$((0x40000000))   # bit30

echo "================= $DEV ================="
echo

TMP=$(mktemp /tmp/synoflags-root.XXXXXX)
if ! btrfs inspect-internal dump-tree -t root "$DEV" >"$TMP" 2>/dev/null; then
  echo "dump-tree 解析 root tree 失败（该设备可能无法读）"
  rm -f "$TMP"; exit 1
fi

# ---------- 1) root tree：root item 的 flags ----------
# 只认 ROOT_ITEM 的 flags（那一行带 last_snapshot）；叶子头行的 flags 0x1(WRITTEN)
# 是叶子块状态，不是 root item 的标志位，混进来就会误判。
echo "--- 1) root tree：root item 的 flags 分布 ---"
grep -oE 'last_snapshot [0-9]+ flags 0x[0-9a-fA-F]+' "$TMP" \
  | sed -E 's/.*flags //' | sort | uniq -c | sort -rn
ROOT_TOTAL=$(grep -cE 'last_snapshot [0-9]+ flags 0x' "$TMP")
echo "root item 总数: $ROOT_TOTAL"
echo

# ---------- 2) root tree：带私有 root flag 的 root item ----------
echo "--- 2) root tree：带【私有】root flag 的 root item（定位是哪些子卷） ---"
ROOT_BAD=$(awk -v bit="$PRIVATE_ROOT_BIT" '
  /item [0-9]+ key \(/ { key=$0; sub(/^[[:space:]]*item [0-9]+ key /,"",key); sub(/\) .*/,")",key) }
  /last_snapshot [0-9]+ flags 0x/ {
    f=$0; sub(/.*flags /,"",f); sub(/\(.*/,"",f); cnt++
    if (f != "0x0" && f != "0x1") print "  " key "  flags " f
  }
  END { if (!cnt) print "  （没有找到 ROOT_ITEM，root tree 解析异常）" }
' "$TMP")
echo "$ROOT_BAD" | grep -q 'flags 0x' && ROOT_DIRTY=1 || ROOT_DIRTY=0
[ "$ROOT_DIRTY" = 0 ] && ROOT_BAD="  （全部是标准值 0x0/0x1，root 层干净）"
echo "$ROOT_BAD"
echo

# ---------- 2b) 这张盘上有哪些数据树 ----------
# 系统树是 1..11（ROOT/EXTENT/CHUNK/DEV/FS/CSUM/QUOTA/UUID/FREE_SPACE…），
# 其余 id 才是数据树/快照/死根残留。
# 注意不能写成 ">=256"：群晖确实会留下 200/201/202 这类 <256 的 root item
# （多半是删快照后的死根，dump-tree 读不出来），漏掉反而让人以为盘上只有一棵树。
echo "--- 2b) root tree 里的数据树（已滤除 1..11 号系统树） ---"
TREE_IDS=$(grep -oE 'key \([0-9]+ ROOT_ITEM 0\)' "$TMP" \
  | sed -E 's/key \(([0-9]+).*/\1/' | sort -n | uniq \
  | awk '$1>11 { printf "%s ", $1 }')
echo "  数据树 / 快照 / 残留 root item 的 ID: ${TREE_IDS:-（无）}"
echo "  → 不带树号直接再跑一次本脚本，就会逐棵统计这些 ID 的 inode 层"
echo
rm -f "$TMP"

# ---------- 3) 抽样 fs tree：inode item 的 flags ----------
shift
# 不给树号就自动扫 2b 列出的全部数据树。
# 为什么不让用户手动传：他得先用一条带 \( 的 grep 把 ID 拼出来，粘进终端时
# 括号很容易被吃掉（真机 2026-10-31：`\(` 变成 `$(`，ids 为空，脚本静默跑了 0 棵树）。
if [ "$#" -eq 0 ] && [ -n "${TREE_IDS// /}" ]; then
  # shellcheck disable=SC2086
  set -- $TREE_IDS
  echo "--- 3) 未指定树号，自动扫描 2b 列出的全部 $# 棵树 ---"
  echo
fi
if [ "$#" -eq 0 ]; then
  echo "--- 3) 没有可扫的数据树 ---"
fi
INODE_DIRTY_TOTAL=0
FS_TOTAL_SUM=0
FS_PARSED=0
for RID in "$@"; do
  echo "--- 3) fs tree $RID：inode item 的 flags 分布（流式统计，可能较慢） ---"
  ERRF=/tmp/synoflags-err.$$
  DST=/tmp/synoflags-fs.$$
  # 排除 `leaf N flags 0x1(WRITTEN) backref revision 1` 这类叶子头行
  btrfs inspect-internal dump-tree -t "$RID" "$DEV" 2>"$ERRF" | awk '
        /^leaf / { next }
        match($0, /flags 0x[0-9a-fA-F]+/) {
          v = substr($0, RSTART+6, RLENGTH-6); c[v]++; total++
        }
        END { for (v in c) printf "%d %s\n", c[v], v; printf "%d TOTAL\n", total }
      ' | sort -rn > "$DST" 2>/dev/null
  rc=${PIPESTATUS[0]}

  if [ "$rc" -ne 0 ]; then
    echo "  ✗ 读不出来（dump-tree 退出码 $rc）：$(head -1 "$ERRF" 2>/dev/null)"
    echo "    两种可能：① 删快照留下的死根（root item 还在、数据块已回收）；"
    echo "    ② btrfs-progs 不接受直接指定该类树号（尤其 <256 的）。以 stderr 为准，别猜。"
    rm -f "$ERRF" "$DST"
    continue
  fi
  tree_total=$(awk '$2=="TOTAL"{print $1}' "$DST")
  if [ "${tree_total:-0}" -eq 0 ]; then
    echo "  ✗ 该树解析出 0 个 inode item —— 空树或残留 root item，不是有数据的子卷。"
    rm -f "$ERRF" "$DST"
    continue
  fi
  while read -r n v; do
    [ "$v" = "TOTAL" ] && continue
    num=$((v))
    if (( num & PRIVATE_INODE_BIT )); then
      printf '  %6d 个 inode  %-12s ← 含群晖私有位（0x%x）\n' "$n" "$v" "$PRIVATE_INODE_BIT"
      INODE_DIRTY_TOTAL=$((INODE_DIRTY_TOTAL + n))
    else
      printf '  %6d 个 inode  %-12s\n' "$n" "$v"
    fi
  done < "$DST"
  printf '  （该树 inode item 合计 %s 个）\n' "$tree_total"
  FS_TOTAL_SUM=$((FS_TOTAL_SUM + tree_total))
  FS_PARSED=$((FS_PARSED + 1))
  rm -f "$ERRF" "$DST"
  echo
done

# ---------- 4) 子卷 ID → 目录名 ----------
# 子卷的名字记在**父树的 DIR_ITEM** 里：顶盘子卷在 FS_TREE(5)，@syno 下嵌套的
# 在 257。之前只扫 257 所以什么都没打出来。
if [ "$#" -gt 0 ]; then
  echo "--- 4) 子卷 ID → 目录名（扫 FS_TREE(5) 与 257 的 DIR_ITEM） ---"
  NAMES=$(
    for PARENT in 5 257; do
      btrfs inspect-internal dump-tree -t "$PARENT" "$DEV" 2>/dev/null | awk -v p="$PARENT" '
        BEGIN { n=0 }
        /location key \([0-9]+ ROOT_ITEM 0\) type DIR/ {
          line=$0; sub(/.*location key \(/,"",line); sub(/ ROOT_ITEM.*/,"",line); n=line; next
        }
        n && /^[[:space:]]*name: / {
          name=$0; sub(/.*name: /,"",name)
          printf "  子卷 %s = %s（父树 %s）\n", n, name, p; n=0
        }
      '
    done
  )
  if [ -n "$NAMES" ]; then
    printf '%s\n' "$NAMES"
  else
    echo "  （5/257 里没有任何指向子卷的目录项）"
  fi
  # 明确点名"哪些 ID 压根没有目录名"——这类树多半是快照，或者 DSM 用默认子卷直接挂载
  for id in $TREE_IDS; do
    printf '%s\n' "$NAMES" | grep -q "子卷 ${id} " || echo "  ⚠ 子卷 $id 没有目录名（未出现在 5/257 的目录项里）"
  done
  echo
fi

# ---------- 4b) 每棵树里装了什么（顶层目录项） ----------
# 名字查不到时，就靠"这棵树里有什么"来认：是共享文件夹（能看到你的目录名）
# 还是快照/残留（看到的是老内容）。dump-tree 边读边匹配，凑够 12 个目录项就
# 主动退出（awk exit 会让 btrfs 收到 SIGPIPE 提前结束），所以大树上也很快。
if [ "$#" -gt 0 ]; then
  echo "--- 4b) 每棵树根目录下的条目（最多 12 个，判断它是数据还是快照） ---"
  for RID in $TREE_IDS; do
    ITEMS=$(btrfs inspect-internal dump-tree -t "$RID" "$DEV" 2>/dev/null | awk '
      /key \(256 (DIR_ITEM|DIR_INDEX)/ { d=1; next }
      d && /^[[:space:]]*name: / {
        sub(/.*name: /, "")
        if ($0 != "." && $0 != "..") { printf "%s  ", $0; n++ }
        d=0
        if (n >= 12) exit
      }
    ')
    printf '  树 %-5s %s\n' "$RID" "${ITEMS:-（读不到根目录项）}"
  done
  echo
fi

# ---------- 5b) 空间占用：决定"能不能靠一次真拷贝重建" ----------
# reflink 搬家只花元数据，几乎不需要空间；但源文件若是 NODATACOW（群晖大量文件
# 都是），FICLONE 可能退化成真拷贝——那时需要预留与数据等量的空闲空间。
echo "--- 5b) 空间占用（dump-super，只读） ---"
btrfs inspect-internal dump-super "$DEV" 2>/dev/null | awk '
  /^total_bytes/  { t=$2 }
  /^bytes_used/   { u=$2 }
  END {
    if (t > 0) {
      printf "  总容量 %.2f TiB，已用 %.2f TiB，剩余 %.2f TiB（%.0f%% 已用）\n",
             t/1024/1024/1024/1024, u/1024/1024/1024/1024, (t-u)/1024/1024/1024/1024, u*100/t
    } else {
      print "  （没读到 total_bytes，dump-super 可能不支持该设备）"
    }
  }'
echo

# ---------- 5) 子卷清单：给每棵树一个名字 ----------
# 第 4 段是从「父树的 DIR_ITEM」反查名字，查不到不代表树没用——DSM 的默认子卷
# 名字记在 root tree 的 ROOT_TREE_DIR（name: default），快照的名字又记在别处。
# 这里直接让 btrfs-progs 自己走一遍（纯用户态，不受 tree-checker 影响）。
echo "--- 5) 子卷清单（btrfs restore -l，纯用户态只读） ---"
SUBV_OUT=/tmp/synoflags-subv.$$
SUBV_ERR=/tmp/synoflags-subv-err.$$
if btrfs restore -l "$DEV" >"$SUBV_OUT" 2>"$SUBV_ERR"; then
  sed -n '1,80p' "$SUBV_OUT"
else
  echo "  ✗ btrfs restore -l 失败（退出码 $?）：$(head -2 "$SUBV_ERR")"
fi
rm -f "$SUBV_OUT" "$SUBV_ERR"
echo

# ---------- 判定 ----------
echo "===== 判定（全程只读，盘上零改动）====="
if [ "$ROOT_DIRTY" = 1 ]; then
  echo "  root 层：有私有 root flag（① 类）——主线内核连树都进不去，只能换环境处理。"
else
  echo "  root 层：干净（无 ① 类问题）。"
fi
printf '  采样 %d 棵树（成功解析 %d 棵），合计 %d 个 inode item\n' "$#" "$FS_PARSED" "$FS_TOTAL_SUM"
if [ "$INODE_DIRTY_TOTAL" -gt 0 ]; then
  PCT=$((INODE_DIRTY_TOTAL * 100 / (FS_TOTAL_SUM > 0 ? FS_TOTAL_SUM : 1)))
  echo "  inode 层：共 $INODE_DIRTY_TOTAL 个 inode 带私有位，占采样总量 ${PCT}%。"
  if [ "$ROOT_DIRTY" = 0 ]; then
    echo "  这种「root 干净、只有 inode 带标」的盘，可在旧内核（DSM7 或 4.15.0-20）里"
    echo "  挂上后按子卷重建，再回飞牛接管——不必整盘回群晖。详见 docs/rescue-vm-cleanup.md。"
  fi
  echo "  提示：只需重建「带标」的那几棵子树，干净的子树不用动。"
else
  echo "  inode 层：采样范围内未见私有位。"
fi
echo
echo "把输出完整贴回来，用于确定清除方案与工作量。"
