#!/usr/bin/env bash
# 共享文件夹注册机制探查·第二轮（只读）。
#
# 第一轮（probe-shares.sh）三个发现：
#   1. probe-test 落在了 /vol2/1000/ ——飞牛把用户共享文件夹组织在 /volN/<uid>/ 下
#   2. trim 库全表 dump 因脚本 bug 跳过（表清单取到的是表头）——本轮修复
#   3. 【警报】mount 表只剩 2 行：sde 的 id=3 注册行消失了（20:31 还在）——本轮核查挂载状态
#
# 用法：sudo bash probe-shares2.sh > p2.txt 2>&1

set -uo pipefail

echo "########## 0. 时间 ##########"
date

psql_q() {
  if sudo -u postgres psql -t -A -P pager=off -c "$1" 2>/dev/null; then
    return 0
  fi
  runuser -u postgres -- psql -t -A -P pager=off -c "$1" 2>&1
}

echo "
########## 1. PostgreSQL 全部 database ##########"
psql_q "SELECT datname FROM pg_database WHERE NOT datistemplate;"

echo "
########## 2. trim 库全表 dump（修复版：-t -A 纯数据）##########"
TABLES="$(psql_q "SELECT string_agg(relname, ' ') FROM pg_stat_user_tables WHERE schemaname='public';")"
echo "表清单: $TABLES"
for t in $TABLES; do
  echo "---------- 表: $t ----------"
  psql_q "SELECT * FROM \"$t\";" || true
done

echo "
########## 3. 全库搜索 probe-test / MediaData1（定位共享文件夹注册写入点）##########"
for db in $(psql_q "SELECT datname FROM pg_database WHERE NOT datistemplate;"); do
  echo "---------- 库: $db ----------"
  # 遍历该库全部表，找含 probe-test 的行
  sudo -u postgres psql -t -A -P pager=off -d "$db" -c "
    SELECT table_schema, table_name FROM information_schema.tables
    WHERE table_schema NOT IN ('pg_catalog','information_schema');" 2>/dev/null | \
  while IFS='|' read -r sch tbl; do
    [ -z "$tbl" ] && continue
    hit=$(sudo -u postgres psql -t -A -P pager=off -d "$db" -c \
      "SELECT count(*) FROM \"$sch\".\"$tbl\" WHERE concat_ws(' ', *) LIKE '%probe-test%';" 2>/dev/null)
    [ "$hit" != "0" ] && [ -n "$hit" ] && echo "  命中: $sch.$tbl (probe-test x$hit)" && \
      sudo -u postgres psql -P pager=off -d "$db" -c "SELECT * FROM \"$sch\".\"$tbl\" WHERE concat_ws(' ', *)::text LIKE '%probe-test%';" 2>/dev/null
  done
done

echo "
########## 4. 当前挂载与卷状态（核查 sde / mount 表消失问题）##########"
echo "----- mount | grep vol -----"
mount | grep -E '/vol' || true
echo "----- /proc/mdstat -----"
cat /proc/mdstat
echo "----- lvs -----"
lvs 2>&1 || true
echo "----- vgs -----"
vgs 2>&1 || true
echo "----- dmsetup ls -----"
dmsetup ls 2>&1 || true
echo "----- mount 表（trim 库）-----"
psql_q "SELECT id, uuid FROM mount ORDER BY id;"

echo "
########## 5. 飞牛存储/文件服务日志（最近报错）##########"
ls /var/log/ 2>/dev/null | head -30
for f in /var/log/trim*.log /var/log/dsmgr*.log /var/log/synofs*.log; do
  [ -f "$f" ] && { echo "----- $f (tail 30) -----"; tail -30 "$f"; }
done

echo "
########## 6. /vol2/1000 与 /vol1/1000 内容对照（飞牛共享文件夹组织方式）##########"
echo "----- /vol2/1000 -----"
ls -la /vol2/1000/ 2>&1 | head -15
echo "----- /vol1/1000 -----"
ls -la /vol1/1000/ 2>&1 | head -15

echo "
########## 探查结束 ##########"
