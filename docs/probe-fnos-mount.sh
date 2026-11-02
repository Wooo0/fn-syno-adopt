#!/usr/bin/env bash
# 探查飞牛如何解析 mount 表的 uuid（全程只读，不写任何东西）。
#
# 背景（2026-10-30 真机现象）：直建 btrfs（/dev/md2，无 LVM 层）接管后，
# mount 表注册了 uuid=/dev/md2 的一行，卷挂载正常、本工具显示"已挂载/已注册"，
# 但飞牛存储管理器把「存储空间 2」标成 **已移除！尝试修复**。
# 推测：飞牛拿 mount.uuid 去解析设备时只认 LVM 命名（trim_xxx-0 -> /dev/mapper/...），
# /dev/mdN 解析不到就当作"盘被物理移除"。
#
# 本脚本要回答三个问题：
#   Q1 trim 库里除了 mount 还有哪些表（是否存在"池"层需要一并注册）？
#   Q2 飞牛的存储服务如何把 uuid 解析成设备节点（认哪些前缀/格式）？
#   Q3 存储空间 2 的"已移除"判定依据（哪张表哪一行）？
#
# 用法：在飞牛 SSH 里  sudo bash probe-fnos-mount.sh > probe-out.txt 2>&1
# 然后把 probe-out.txt 发回来分析。

set -uo pipefail
# 不用 set -e：任何一条查不到都要继续跑完剩下的探查

echo "########## 0. 系统信息 ##########
$(uname -a)
$(cat /etc/os-release 2>/dev/null | head -2)
date: $(date)

########## 1. trim 库全部表（含行数估计）##########"

psql_q() {
  # 优先 sudo -u postgres，失败降级 runuser（与本工具后端同策略）
  if sudo -u postgres psql -d trim -P pager=off -c "$1" 2>/dev/null; then
    return 0
  fi
  runuser -u postgres -- psql -d trim -P pager=off -c "$1" 2>&1
}

psql_q "SELECT relname AS 表名, n_live_tup AS 行数
        FROM pg_stat_user_tables ORDER BY relname;"

echo "
########## 2. mount 表全部行（我们插的是 id=2 uuid=/dev/md2）##########"
psql_q "SELECT * FROM mount ORDER BY id;"

echo "
########## 3. 疑似「存储池/空间/磁盘」相关表的完整内容 ##########
（表名是猜的，跑完第 1 节后按实际表名补充；空表/不存在的表会报错，忽略即可）"
for t in pool pools storage_pool storage_pools space spaces volume volumes \
         disk disks md raid pool_member pool_members storagepool storagepools; do
  echo "--- 表 $t:"
  psql_q "SELECT * FROM $t LIMIT 30;"
done

echo "
########## 4. 当前真实挂载（vol/mnt 相关）##########"
grep -E '/vol|/mnt' /proc/self/mounts

echo "
########## 5. 设备节点现状 ##########
--- /dev/mapper:
ls -l /dev/mapper/ 2>&1
"
ls -l /dev/mapper/ 2>&1
echo "--- /dev/md*:"
ls -l /dev/md* 2>&1
echo "--- pvs/lvs:"
pvs 2>&1
lvs 2>&1

echo "
########## 6. 飞牛存储服务进程与二进制 ##########"
ps ax 2>/dev/null | grep -iE 'trim|storage' | grep -v grep
echo "--- /usr/trim 下的可执行/服务:"
find /usr/trim -maxdepth 3 \( -type f -o -type l \) 2>/dev/null | grep -iE 'storage|mount|pool' | head -20

echo "
########## 7. 存储服务二进制里的 uuid 解析线索 ##########
（在二进制里搜设备路径模式：若只出现 /dev/mapper/ 而没有 /dev/md，则坐实'只认 LVM 命名'）"
for b in $(find /usr/trim -maxdepth 4 -type f -size +1M 2>/dev/null | grep -iE 'storage|trim' | head -6); do
  echo "=== 二进制: $b"
  strings "$b" 2>/dev/null | grep -E '^/dev/|trim_%|trim_.*-0|/dev/mapper|/dev/md' | sort -u | head -30
done

echo "
########## 8. trim 服务的配置/日志里关于 mount 或 存储空间2 的报错 ##########"
journalctl --no-pager -n 200 2>/dev/null | grep -iE 'mount|storage|space|removed' | tail -40
for f in $(find /var/log -maxdepth 2 -name '*trim*' -o -maxdepth 2 -name '*storage*' 2>/dev/null | head -5); do
  echo "--- 日志: $f"; tail -30 "$f" 2>/dev/null
done

echo "
########## 探查结束，把以上全部输出发回 ##########"
