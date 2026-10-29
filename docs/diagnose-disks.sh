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

say "8) 未出现在任何 PV 里的群晖特征分区（漏盘元凶候选）"
comm -23 \
  <( { blkid -t TYPE=linux_raid_member -o device 2>/dev/null; \
       blkid -t TYPE=LVM2_member      -o device 2>/dev/null; } | sort -u) \
  <( pvs --noheadings -o pv_name 2>/dev/null | awk '{print $1}' | sort -u)
echo "  ↑ 有输出 = 这些分区上有群晖元数据但没成为 PV（md 未组装 / 未 pvscan）"

say "9) LVM 扫描状态（这一步会刷新缓存，只读，安全）"
vgscan 2>&1 | tail -20
