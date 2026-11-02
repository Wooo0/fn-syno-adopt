#!/usr/bin/env bash
# 试验：把 /dev/md2 用 dm-linear 透明包装成飞牛原生命名 /dev/mapper/trim_<uuid>-0，
# 再按原生格式注册 mount 表（pool_type=0），验证存储管理器是否认账。
#
# 安全性：
#   - dm-linear 是纯透明映射（LVM 底层就是这么实现的），不读不写盘上任何数据
#   - 全程可逆，撤销命令见文末；撤销后系统回到回滚后的干净状态
#
# 用法：sudo bash try-dmlinear.sh
set -euo pipefail

DEV=/dev/md2
[ -b "$DEV" ] || { echo "找不到 $DEV"; exit 1; }

# md 阵列 UUID（冒号转下划线），拼出飞牛风格设备名 trim_<uuid>-0
MDUUID=$(mdadm --detail "$DEV" | awk '/UUID :/{print $3}')
# 冒号与连字符都转下划线，对齐原生全下划线风格（真机教训：只替换 - 会留下冒号）
VGNAME="trim_$(echo "$MDUUID" | tr ':-' '__')"
DMNAME="${VGNAME}-0"
echo "== md2 UUID: $MDUUID"
echo "== 目标设备名: /dev/mapper/$DMNAME"

echo "
== 1. 创建 dm-linear 映射（透明包装，不动数据）=="
if dmsetup info "$DMNAME" >/dev/null 2>&1; then
  echo "映射已存在，跳过创建"
else
  SECTORS=$(blockdev --getsize "$DEV")
  echo "0 $SECTORS linear $DEV 0" | dmsetup create "$DMNAME"
  echo "已创建（$SECTORS 扇区）"
fi
ls -l "/dev/mapper/$DMNAME"

echo "
== 2. 注册 mount 表（id=2, uuid=$DMNAME, pool_type=0 同原生）=="
PSQL="sudo -u postgres psql -d trim -P pager=off -c"
$PSQL "DELETE FROM mount WHERE id=2;"
$PSQL "INSERT INTO mount (id,uuid,comment,sync_speed_min,sync_speed_max,stripe_cache_size,warn_frsize,grow,state,pool_type) VALUES (2,'$DMNAME','',-1,-1,4096,10,'',0,0);"
$PSQL "SELECT id,uuid,pool_type FROM mount ORDER BY id;"

echo "
== 3. 挂载 /vol2（经包装设备）=="
mkdir -p /vol2
if ! mountpoint -q /vol2; then
  mount -o rw,noatime,space_cache=v2 "/dev/mapper/$DMNAME" /vol2
fi
mount | grep /vol2

echo "
== 4. 顺手挖一下 pool_type 枚举线索（函数名里找 attach/pool 家族）=="
strings /usr/trim/bin/handlers/storage.hdl | grep -iE 'attach|detach' | grep -viE 'simdutf|lvm' | sort -u | head -20

echo "
============================================================"
echo "现在去飞牛「存储管理器」刷新页面，看「存储空间 2」是否正常显示（不再显示已移除）。"
echo ""
echo "撤销试验（恢复到回滚后的干净状态）："
echo "  umount /vol2"
echo "  dmsetup remove $DMNAME"
echo "  sudo -u postgres psql -d trim -c \"DELETE FROM mount WHERE id=2;\""
