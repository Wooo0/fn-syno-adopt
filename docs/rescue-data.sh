#!/bin/bash
# 只读数据抢救 —— 全程不写源盘，只往目标目录写
#
# 适用：btrfs 树根损坏导致无法挂载（open_ctree failed / couldn't read tree root）。
# btrfs restore 走用户态、不依赖挂载，能绕过损坏的树根把文件提出来。
#
# 用法：
#   sudo bash rescue-data.sh                        # 只列出各设备可用 root
#   sudo bash rescue-data.sh /dev/md2               # 列出该设备的 root
#   sudo bash rescue-data.sh /dev/md2 /dest 257     # 按 root 257 恢复到 /dest
#   sudo bash rescue-data.sh /dev/md2 /dest 257 -D  # 先空跑，看能救多少（强烈建议先做）
#
# ⚠️ 永远不要对源盘执行写操作。

set -u
say() { printf '\n\033[1m===== %s =====\033[0m\n' "$1"; }

DEV=${1:-}
DEST=${2:-}
ROOT=${3:-}
DRY=${4:-}

# ---------- 模式一：没有任何参数 —— 概览所有问题盘 ----------
if [ -z "$DEV" ]; then
  say "概览：各问题盘的可用 root"
  for d in /dev/mapper/vg1-volume_7 /dev/md2 /dev/md3; do
    [ -e "$d" ] || continue
    echo
    echo "----- $d -----"
    btrfs restore -l "$d" 2>&1 | head -20
  done
  cat <<'TIP'

下一步：
  1. 挑一个 root id（群晖数据通常在 256 / 257 附近的那个，或者文件最多的）
  2. 先空跑看能救多少：
       sudo bash rescue-data.sh /dev/md2 /dest 257 -D
  3. 确认后真跑：
       sudo bash rescue-data.sh /dev/md2 /dest 257
TIP
  exit 0
fi

[ -e "$DEV" ] || { echo "设备不存在: $DEV"; exit 1; }

# ---------- 模式二：只列 root ----------
if [ -z "$DEST" ] || [ -z "$ROOT" ]; then
  echo "=== $DEV 的可用 root ==="
  btrfs restore -l "$DEV" 2>&1
  exit 0
fi

# ---------- 模式三：恢复 ----------
say "从 $DEV 的 root $ROOT 恢复${DRY:+（空跑，不写盘）}"

if [ -n "$DRY" ]; then
  btrfs restore -D -v -r "$ROOT" "$DEV" /tmp/__rescue_dryrun 2>&1 | tail -15
  echo
  echo "↑ 空跑结果，确认无误后去掉末尾的 -D 真跑"
  exit 0
fi

mkdir -p "$DEST" || exit 1
echo "目标目录：$DEST"
echo "剩余空间：$(df -h "$DEST" | tail -1 | awk '{print $4}')"
read -r -p "确认开始？[y/N] " ans
[ "$ans" = "y" ] || { echo "已取消"; exit 0; }

btrfs restore -v -r "$ROOT" "$DEV" "$DEST" 2>&1 | tail -30

say "完成"
echo "恢复结果："
du -sh "$DEST" 2>/dev/null
find "$DEST" -maxdepth 1 2>/dev/null | head -20
