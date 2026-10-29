#!/bin/bash
# fn-syno-adopt 漏盘诊断 —— 只读，不修改任何东西
# 用法：sudo bash diagnose-disks.sh
# 目的：候选列表只来自 `lvs`，所以"少了盘"一定是某一层断了。
#       本脚本从物理盘 → md 阵列 → PV → VG/LV 逐层打印，定位断点。

say() { printf '\n\033[1m===== %s =====\033[0m\n' "$1"; }

say "1) 物理盘（整盘，看有没有少透传）"
lsblk -d -o NAME,SIZE,TYPE,MODEL,SERIAL,WWN 2>/dev/null
echo "整盘数量: $(lsblk -dn -o NAME,TYPE 2>/dev/null | awk '$2=="disk"{n++}END{print n+0}')"

say "2) 群晖特征分区"
echo "-- linux_raid_member 分区（群晖几乎每块盘的数据区都包一层 md）--"
blkid -t TYPE=linux_raid_member -o device 2>/dev/null | sort
echo "数量: $(blkid -t TYPE=linux_raid_member -o device 2>/dev/null | wc -l)"
echo
echo "-- LVM2_member 分区（裸 PV，不经 md）--"
blkid -t TYPE=LVM2_member -o device 2>/dev/null | sort
echo "数量: $(blkid -t TYPE=LVM2_member -o device 2>/dev/null | wc -l)"

say "3) 已组装的 md 阵列"
cat /proc/mdstat 2>/dev/null
echo "活跃 md 数量: $(ls /sys/block 2>/dev/null | grep -c '^md')"

say "4) 能扫到但没组装的阵列（这一层最可能是漏盘原因）"
mdadm --examine --scan 2>&1
echo "未组装 ARRAY 行数: $(mdadm --examine --scan 2>/dev/null | grep -c '^ARRAY')"

say "5) PV / VG / LV"
echo "-- pvs --"
pvs -o pv_name,pv_size,vg_name,vg_uuid --separator ' | ' 2>&1
echo "-- vgs --"
vgs -o vg_name,vg_uuid,vg_size,vg_attr --separator ' | ' 2>&1
echo "-- lvs（含未激活）--"
lvs -a -o vg_name,vg_uuid,lv_name,lv_size,lv_attr,lv_dm_path --separator ' | ' 2>&1

say "6) partial 卷组（缺 PV 的 VG，lvs 默认隐藏它的 LV，这是隐性漏盘）"
vgs -o vg_name,vg_uuid,vg_attr,vg_missing_pv_count --separator ' | ' --partial 2>&1
echo "-- 用 --partial 再看一次 LV（这里多出来的就是被默认隐藏的）--"
lvs --partial -a -o vg_name,vg_uuid,lv_name,lv_size,lv_attr,lv_dm_path --separator ' | ' 2>&1

say "7) 关键对照（重点看这三行）"
U=$(vgs --noheadings -o vg_uuid 2>/dev/null | awk '{print $1}' | sort -u | grep -c .)
N=$(vgs --noheadings -o vg_name  2>/dev/null | awk '{print $1}' | sort -u | grep -c .)
echo "VG 按 UUID 去重: $U"
echo "VG 按名字去重  : $N"
if [ "$U" != "$N" ]; then
  echo "  ⚠ 两者不等 → 存在重名卷组（群晖盘都叫 vg1/vg2/vg3），LVM 按名字会折叠，必须按 UUID 查"
else
  echo "  ✓ 没有重名卷组"
fi
D=$(lvs --noheadings -o lv_name 2>/dev/null | awk '{print $1}' \
    | grep -vE '^syno_vg_reserved_area$' | grep -cE '^(lv|volume_[0-9]+)$')
echo "群晖数据 LV 数量（已排除 12M 保留区）: $D"
echo "  ↑ 如果这个数 < 你实际接入的盘数，回去看第 4 步：多半是 md 没组装"

say "8) 每个已组装 md 阵列的归宿（漏盘元凶就在这一节）"
# 旧写法拿「分区」和「/dev/mdX」做差集，两套路径体系永远不匹配，等于没输出。
# 正确的问法是：每个活跃 md 阵列当前是不是 PV？不是的话它上面到底是什么？
PVSET=$(pvs --noheadings -o pv_name 2>/dev/null | awk '{print $1}' | sort -u)
for md in $(ls /sys/block 2>/dev/null | grep '^md' | sort -V); do
  dev=/dev/$md
  [ -b "$dev" ] || continue
  if echo "$PVSET" | grep -qx "$dev"; then
    vg=$(pvs --noheadings -o vg_name "$dev" 2>/dev/null | awk '{print $1}')
    printf '  ✅ %-10s → PV，属于卷组 %s\n' "$dev" "$vg"
  else
    # 不是 PV：这一块盘就不会出现在 lvs 里，属于"另一种布局"
    fs=$(blkid -o value -s TYPE "$dev" 2>/dev/null)
    sz=$(lsblk -dn -o SIZE "$dev" 2>/dev/null)
    printf '  ❌ %-10s → 不是 PV！容量 %s，上面直接是：%s\n' \
      "$dev" "${sz:-?}" "${fs:-（无文件系统签名）}"
    case "$fs" in
      btrfs)
        echo "       ↳ btrfs 直接建在阵列上（没有 LVM 层），当前版本不支持接管"
        btrfs subvolume list "$dev" 2>&1 | head -5 | sed 's/^/       /'
        ;;
      ext4) echo "       ↳ ext4 直接建在阵列上（没有 LVM 层）" ;;
    esac
  fi
done
echo "  ↑ 出现 ❌ 的行 = 已组装但没有 LVM 的阵列，这就是界面上少掉的盘"

say "9) LVM 扫描状态（这一步会刷新缓存，只读，安全）"
vgscan 2>&1 | tail -20

say "10) btrfs 子卷读不到的三类根因（界面报 not a directory / can't access 时看这里）"
echo "-- btrfs-progs 是否装了、版本多少 --"
command -v btrfs >/dev/null && btrfs --version || echo "btrfs 命令不存在 → 系统缺 btrfs-progs"
echo
echo "-- 群晖卷的设备节点到底在不在（关键）--"
# 坑：GNU stat 默认用 lstat，对符号链接只显示 "symbolic link"，看不出目标类型。
# 必须用 stat -L 跟随链接，才能判断 /dev/dm-N 是不是真的块设备。
for m in /dev/mapper/vg*-*; do
  [ -e "$m" ] || continue
  case "$m" in *syno_vg_reserved_area*) continue;; esac
  real=$(readlink -f "$m")
  if [ -b "$real" ]; then
    printf '  ✅ %s -> %s [%s]\n' "$m" "$real" "$(stat -L -c '%F' "$m" 2>/dev/null)"
  else
    printf '  ❌ %s -> %s 目标不是块设备（悬空链接/节点缺失）\n' "$m" "${real:-（无法解析）}"
  fi
done
echo
echo "-- 每块群晖数据卷实际能不能读子卷（必须走挂载法，块设备法在 v6.2 上必失败）--"
lvs --noheadings -o vg_name,lv_name,lv_dm_path 2>/dev/null | while read -r vg lv dm; do
  case "$lv" in syno_vg_reserved_area) continue;; esac
  case "$vg" in trim_*) continue;; esac
  d=$(mktemp -d /tmp/probe10-XXXX)
  btrfs device scan "$dm" >/dev/null 2>&1
  if mount -t btrfs -o ro,subvolid=5 "$dm" "$d" 2>/dev/null; then
    printf '  ✅ %s/%s 正常挂载，子卷：%s\n' "$vg" "$lv" \
      "$(btrfs subvolume list "$d" 2>/dev/null | awk '{print $NF}' | tr '\n' ' ')"
    umount "$d" 2>/dev/null
  elif mount -t btrfs -o ro,usebackuproot,subvolid=5 "$dm" "$d" 2>/dev/null; then
    printf '  ⚠️  %s/%s **主树根损坏**，靠备份树根才挂上，子卷：%s\n' "$vg" "$lv" \
      "$(btrfs subvolume list "$d" 2>/dev/null | awk '{print $NF}' | tr '\n' ' ')"
    umount "$d" 2>/dev/null
  else
    printf '  ❌ %s/%s 挂载失败：%s\n' "$vg" "$lv" \
      "$(mount -t btrfs -o ro,subvolid=5 "$dm" "$d" 2>&1 | head -1)"
  fi
  rmdir "$d" 2>/dev/null
done
echo
echo "-- 内核 btrfs 模块（subvolume list 走 ioctl，模块没加载会失败）--"
lsmod 2>/dev/null | grep -w btrfs || echo "btrfs 内核模块未加载：modprobe btrfs"
echo
echo "若上面全是『失败』，一次激活全部群晖卷组（只建设备映射，不动数据）："
echo "  sudo vgchange -ay"

say "11) 子卷读不到的进阶排查（挂载法也失败时看这里）"
echo "-- 手动复现：带 -t btrfs 挂载，能看到真正的报错（不加 -t 只报 can't read superblock）--"
for dm in $(lvs --noheadings -o lv_dm_path 2>/dev/null | awk '{print $1}' | grep -v reserved) /dev/md2 /dev/md3; do
  [ -e "$dm" ] || continue
  d=$(mktemp -d /tmp/probe-XXXX)
  btrfs device scan "$dm" >/dev/null 2>&1
  if mount -t btrfs -o ro,subvolid=5 "$dm" "$d" 2>/tmp/mnt.err; then
    printf '  ✅ %s 挂载成功，子卷：%s\n' "$dm" "$(btrfs subvolume list "$d" 2>/dev/null | awk '{print $NF}' | tr '\n' ' ')"
    umount "$d" 2>/dev/null
  else
    printf '  ❌ %s 挂载失败：%s\n' "$dm" "$(cat /tmp/mnt.err | head -2 | tr '\n' ' ')"
  fi
  rmdir "$d" 2>/dev/null
done
echo
echo "-- 内核最近的相关报错（I/O error / btrfs 报错都在这里）--"
dmesg 2>/dev/null | tail -40 | grep -iE "btrfs|I/O error|ata|scsi|md[0-9]" || echo "（dmesg 无相关输出或权限不足）"
echo
echo "-- 各盘 SMART 健康（出现 Reallocated/Pending/Uncorrectable 就要警惕）--"
command -v smartctl >/dev/null || { echo "未安装 smartctl：apt install smartmontools"; return 2>/dev/null || exit 0; }
for d in sda sdb sdc sdd sde sdf sdg; do
  [ -b "/dev/$d" ] || continue
  printf '%-4s %s\n' "/dev/$d" "$(smartctl -H "/dev/$d" 2>/dev/null | grep -i 'overall-health' | head -1)"
done
