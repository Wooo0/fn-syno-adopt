#!/usr/bin/env bash
# 第二轮探查：飞牛如何把 mount.uuid 解析成设备（全程只读）。
#
# 第一轮结论：
#   - trim 库没有"池"表，存储空间 = mount 表 + 设备解析
#   - 原生 uuid 恒为 trim_xxx-0（md -> LVM -> btrfs），原生 /vol1 挂载用 subvolid=5
#   - /usr/trim/bin/handlers/storage.hdl 官方支持 LVM/ZFS/ext4/md/裸分区多后端
#     → 直建设备应该有合法注册方式，藏在 pool_type 枚举或 uuid 格式里
#
# 本轮目标：
#   Q1 storage.hdl / dsmgr / trim 二进制里的设备路径与 pool_type 线索
#   Q2 dsmgr 的日志里对 /dev/md2 或 mount id=2 的判定
#   Q3 pool_type 的取值枚举（0 是什么，还有哪些）
#
# 用法：sudo bash probe-fnos-mount2.sh > probe2-out.txt 2>&1

set -uo pipefail

echo "########## 1. storage.hdl 的字符串线索 ##########"
for pat in '/dev/md' '/dev/mapper' 'trim_' 'pool_type' 'pooltype' 'pool-type' \
           'lvm' 'mdadm' 'multi device\|btrfs' 'subvolid'; do
  echo "--- 模式: $pat"
  strings /usr/trim/bin/handlers/storage.hdl 2>/dev/null | grep -iE "$pat" | sort -u | head -25
done

echo "
########## 2. dsmgr 的字符串线索 ##########"
for pat in '/dev/md' '/dev/mapper' 'trim_' 'pool_type' 'uuid' 'removed\|missing'; do
  echo "--- 模式: $pat"
  strings /usr/trim/bin/dsmgr 2>/dev/null | grep -iE "$pat" | sort -u | head -25
done

echo "
########## 3. trim 主程序里的 mount 表 SQL 线索 ##########"
strings /usr/trim/bin/trim 2>/dev/null | grep -iE 'FROM mount|UPDATE mount|INSERT INTO mount|pool_type' | sort -u | head -20

echo "
########## 4. 其它 handler 一览（可能不止 storage.hdl）##########"
ls -l /usr/trim/bin/handlers/ 2>/dev/null

echo "
########## 5. dsmgr / 存储相关日志 ##########"
ls -l /usr/trim/logs/ 2>/dev/null | head -30
for f in /usr/trim/logs/dsmgr.log /usr/trim/logs/trim.log /usr/trim/logs/storage*.log; do
  if [ -f "$f" ]; then
    echo "--- $f（最近 80 行，过滤 md2/vol2/removed）:"
    grep -iE 'md2|vol2|removed|missing|uuid' "$f" 2>/dev/null | tail -80
  fi
done

echo "
########## 6. journalctl 里 dsmgr/trim_sac 对存储空间2 的完整判定 ##########"
journalctl --no-pager --since "2026-10-30 02:30" 2>/dev/null | \
  grep -iE 'dsmgr|storage|space 2|vol2|md2' | grep -v 'sudo\|psql' | tail -60

echo "
########## 探查结束 ##########"
