#!/bin/bash
# 救援探查：针对"挂载失败"或"挂载成功但没有子卷"的 btrfs 卷
#
# 背景：群晖老盘常见两类问题
#   1. btrfs 树根损坏 → open_ctree failed / couldn't read tree root
#   2. mdadm 用错超块（盘上有残留）→ data offset 不同 → 整个 btrfs 错位
#      （表现为 "invalid root flags, have 0x... expect mask 0x..."，值完全不像正常字段）
#
# 本脚本对指定设备逐个尝试所有只读读法，全程不写盘。
# 用法：sudo bash probe-damaged.sh

set -u
say() { printf '\n\033[1m===== %s =====\033[0m\n' "$1"; }

# ---------- 1. 逐个设备试各种读法 ----------
probe() {
  dev=$1
  label=$2
  [ -e "$dev" ] || { echo "跳过 $dev（不存在）"; return; }
  echo
  echo "----- $label  ($dev) -----"

  d=$(mktemp -d /tmp/rescue-XXXX)
  ok=0

  # a. 直接挂 FS tree
  if mount -t btrfs -o ro,subvolid=5 "$dev" "$d" 2>/dev/null; then
    echo "  ✅ [a] 直接挂载成功"
    echo "     子卷：$(btrfs subvolume list "$d" 2>/dev/null | awk '{print $NF}' | tr '\n' ' ')"
    echo "     顶层：$(ls -A "$d" 2>/dev/null | tr '\n' ' ')"
    umount "$d" 2>/dev/null; rmdir "$d" 2>/dev/null; return
  fi

  # b. 备份树根（飞牛官方恢复命令用的就是它）
  if mount -t btrfs -o ro,usebackuproot,subvolid=5 "$dev" "$d" 2>/dev/null; then
    echo "  ⚠️ [b] 备份树根挂载成功（主树根已损坏，数据可能不完整）"
    echo "     子卷：$(btrfs subvolume list "$d" 2>/dev/null | awk '{print $NF}' | tr '\n' ' ')"
    echo "     顶层：$(ls -A "$d" 2>/dev/null | tr '\n' ' ')"
    umount "$d" 2>/dev/null; rmdir "$d" 2>/dev/null; return
  fi

  # c. 全救援选项
  if mount -t btrfs -o ro,usebackuproot,rescue=all,subvolid=5 "$dev" "$d" 2>/dev/null; then
    echo "  ⚠️ [c] rescue=all 挂载成功（数据可能不完整）"
    echo "     子卷：$(btrfs subvolume list "$d" 2>/dev/null | awk '{print $NF}' | tr '\n' ' ')"
    umount "$d" 2>/dev/null; rmdir "$d" 2>/dev/null; return
  fi

  # d. 不掛载，直接扫文件树（btrfs restore 能绕过损坏的树根）
  echo "  ❌ 三种挂载方式都失败，试 btrfs restore（不依赖挂载）："
  out=$(btrfs restore -l "$dev" 2>&1 | head -12)
  if [ -n "$out" ]; then
    echo "$out" | sed 's/^/     /'
  else
    echo "     （无输出，文件系统结构已不可读）"
  fi
  rmdir "$d" 2>/dev/null
}

say "1) 逐个尝试读出（只读，全程不改盘）"
probe /dev/mapper/vg1-volume_7  "sda 14.55T"
probe /dev/md2                   "sdc 10.9T"
probe /dev/md3                   "sdd 14.5T"

# ---------- 2. sde：能挂却没子卷，单独看它到底装了什么 ----------
say "2) vg2/volume_6（sde）能挂却没子卷，单独查"
d=$(mktemp -d /tmp/sde-XXXX)
if mount -t btrfs -o ro,subvolid=5 /dev/mapper/vg2-volume_6 "$d" 2>/dev/null; then
  echo "-- FS tree 顶层有什么 --"
  ls -la "$d" 2>&1 | head -20
  echo "-- 全部子卷（含已删除 -a）--"
  btrfs subvolume list -a "$d" 2>&1 | head -20
  echo "-- 默认子卷 --"
  btrfs subvolume get-default "$d" 2>&1
  echo "-- 文件系统信息 --"
  btrfs filesystem show /dev/mapper/vg2-volume_6 2>&1 | head -10
  btrfs filesystem df "$d" 2>&1 | head -10
  umount "$d" 2>/dev/null
else
  echo "挂载失败"
fi
rmdir "$d" 2>/dev/null

# ---------- 3. sda 的内核报错（之前被 md2/md3 刷屏挡住了） ----------
say "3) 单独抓 sda 的内核报错"
dmesg -C 2>/dev/null    # 先清空缓冲，保证下面看到的都是这次的
d2=$(mktemp -d /tmp/sda-XXXX)
mount -t btrfs -o ro,subvolid=5 /dev/mapper/vg1-volume_7 "$d2" 2>/dev/null
dmesg 2>/dev/null | tail -20
umount "$d2" 2>/dev/null; rmdir "$d2" 2>/dev/null

# ---------- 4. md 超块：是不是组错了？ ----------
say "4) mdadm 超块 —— 怀疑组错超块导致 btrfs 整体错位"
for p in /dev/sdc3 /dev/sdd3; do
  [ -e "$p" ] || continue
  echo "--- $p ---"
  mdadm --examine "$p" 2>/dev/null | grep -iE "Data Offset|UUID|Events|Update Time|Raid Level"
done
echo
echo "--- 当前已组装阵列的实际信息 ---"
mdadm --detail /dev/md2 2>/dev/null | head -14
mdadm --detail /dev/md3 2>/dev/null | head -14

say "5) 如果上面 Data Offset 不为 0，可以这样按偏移绕过 md 头试挂"
cat <<'TIP'
  # 例：data offset = 4096 扇区（每扇区 512B → 2097152 字节）
  OFF=$(mdadm --examine /dev/sdc3 | awk -F': *' '/Data Offset/{print $2}' | awk '{print $1}')
  echo "data offset = $OFF 扇区"
  LO=$(losetup -f --show -o $((OFF*512)) /dev/sdc3)
  mount -t btrfs -o ro,subvolid=5 "$LO" /mnt/rescue && btrfs subvolume list /mnt/rescue
TIP
