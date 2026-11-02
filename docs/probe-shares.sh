#!/usr/bin/env bash
# 探查飞牛把"共享文件夹"注册在哪（全程只读，不写任何东西）。
#
# 背景（2026-10-30）：md2 接管后，数据根子卷挂在 /vol2，里面的 MediaData1/1000
# 就是群晖时代的共享文件夹（嵌套子卷），物理路径已在存储空间 2 里——但飞牛
# "我的文件"不显示它们，因为飞牛只显示**注册过的共享文件夹**，不自动扫描卷下目录。
#
# 目标：搞清楚飞牛把共享文件夹注册在哪（trim 库哪张表？还是独立配置文件），
# 确认后即可在接管流程里安全地自动注册（v0.7 候选功能）。
#
# 用法（两轮对比，定位"注册动作写了哪"）：
#   1) 建测试共享文件夹【之前】跑一次：  sudo bash probe-shares.sh > before.txt 2>&1
#   2) 在飞牛 Web UI 建一个测试共享文件夹
#      （推荐直接对 /vol2/MediaData1 建：名字填 MediaData1，路径选"使用现有目录"）
#   3) 【之后】再跑一次：              sudo bash probe-shares.sh > after.txt 2>&1
#   4) 把两个文件发回来，diff 就是注册写入点
#
# 注意：任何一步报错都继续跑（不 set -e），探查宁多勿缺。

set -uo pipefail

echo "########## 0. 系统信息 ##########
$(uname -a)
date: $(date)
"

psql_q() {
  if sudo -u postgres psql -d trim -P pager=off -c "$1" 2>/dev/null; then
    return 0
  fi
  runuser -u postgres -- psql -d trim -P pager=off -c "$1" 2>&1
}

echo "########## 1. trim 库全部表 ##########"
psql_q "SELECT relname AS 表名, n_live_tup AS 行数
        FROM pg_stat_user_tables ORDER BY relname;"

echo "
########## 2. trim 库每张表全量内容（表都很小，全 dump）##########"
TABLES="$(psql_q "SELECT string_agg(relname, ' ') FROM pg_stat_user_tables WHERE schemaname='public';" | head -1)"
echo "表清单: $TABLES"
for t in $TABLES; do
  echo "---------- 表: $t ----------"
  psql_q "SELECT * FROM $t;" || true
done

echo "
########## 3. trim 库其它 schema 的表 ##########"
psql_q "SELECT table_schema, table_name FROM information_schema.tables
        WHERE table_schema NOT IN ('pg_catalog','information_schema','public')
        ORDER BY 1,2;" || true

echo "
########## 4. Samba 配置（飞牛共享文件夹通常同步写进 smb 段）##########"
for f in /etc/samba/smb.conf /usr/trim/etc/smb.conf /var/etc/smb.conf; do
  [ -f "$f" ] && { echo "----- $f -----"; cat "$f"; }
done
find /etc/samba /usr/trim -maxdepth 3 -name '*.conf*' -o -maxdepth 3 -name '*.ini' 2>/dev/null | head -20

echo "
########## 5. /usr/trim 下疑似共享文件夹配置 ##########"
grep -rliE 'share|folder' /usr/trim/etc /usr/trim/config 2>/dev/null | head -20

echo "
########## 6. 飞牛原生空间顶层（/vol1）对照：飞牛自己怎么组织 ##########"
ls -la /vol1/ 2>/dev/null | head -30

echo "
########## 7. 本机全部卷顶层（看 /vol2 里有什么可注册）##########"
for d in /vol2 /vol3 /vol02; do
  [ -d "$d" ] && { echo "----- $d -----"; ls -la "$d" | head -20; }
done

echo "
########## 8. 最近 2 分钟系统配置类文件变动（辅助定位写入点）##########"
find /etc /usr/trim /var/lib -mmin -2 -type f 2>/dev/null | grep -viE 'log|cache|sock|pid' | head -30

echo "
########## 探查结束 ##########"
