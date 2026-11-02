#!/usr/bin/env bash
# 共享文件夹注册机制探查·第三轮（只读）。
#  - 修复第二轮 psql 漏 -d trim 的 bug，重查 mount 表（sde id=3 是否真消失）
#  - 全盘 grep "probe-test"，直接定位飞牛共享文件夹的注册存储文件
# 用法：sudo bash probe-shares3.sh > p3.txt 2>&1
set -uo pipefail

echo "########## 0. 时间 ##########"
date

echo "
########## 1. trim 库 mount 表（修正 -d trim）##########"
sudo -u postgres psql -d trim -P pager=off -c "SELECT id, uuid FROM mount ORDER BY id;" 2>&1

echo "
########## 2. 全盘 grep probe-test（注册文件直接现形）##########"
# 排除 docker/postgresql 大目录与日志；-a 让 sqlite/二进制里的明文也能命中
grep -rla --exclude-dir=docker --exclude-dir=postgresql --exclude-dir=log \
  --exclude-dir=journal --exclude=*.gz --exclude=*.log \
  "probe-test" /usr/trim /var/lib /etc /var/tmp /root 2>/dev/null | head -20

echo "
########## 3. 二选一落点的 sqlite/db 文件清单 ##########"
find /usr/trim /var/lib -maxdepth 4 \( -name '*.db' -o -name '*.sqlite' -o -name '*.sqlite3' \) \
  --exclude-dir=docker --exclude-dir=postgresql 2>/dev/null | head -20
find /usr/trim /var/lib -maxdepth 4 -name '*.db' -not -path '*docker*' -not -path '*postgresql*' 2>/dev/null | head -20

echo "
########## 4. 飞牛文件服务进程（找负责共享文件夹的服务）##########"
ps aux | grep -iE 'trim|dsmgr|file' | grep -v grep | head -15

echo "
########## 探查结束 ##########"
