#!/bin/bash
# 探测：飞牛的存储空间管理到底认不认「裸设备」（没有 LVM 层）
#
# 背景：群晖老卷（DSM 5/6 早期）的 btrfs 直接建在 md 阵列上，中间没有 LVM。
# 接管这类盘有两条路：
#   A. 飞牛接受裸设备注册 → 零拷贝接管
#   B. 飞牛只认 trim_<uuid> 卷组 → 只能拷贝，或者手动挂到别处访问
# 本脚本用 loop 设备造假盘来试，**完全不碰你的真实数据盘**。
#
# 用法：sudo bash probe-bare-device.sh

set -u
IMG=/tmp/fn-probe.img
LO=""
MOUNT=/vol9          # 用一个当前没被占用的编号，脚本会检查
TMPDIR_MNT=/tmp/fn-probe-mnt
ID=9                 # mount 表 id，脚本会检查冲突

cleanup() {
  echo
  echo "===== 清理 ====="
  mountpoint -q "$MOUNT" 2>/dev/null && umount "$MOUNT" 2>/dev/null
  [ -d "$TMPDIR_MNT" ] && rmdir "$TMPDIR_MNT" 2>/dev/null
  [ -n "$LO" ] && losetup -d "$LO" 2>/dev/null
  rm -f "$IMG"
  echo "已清理 loop 设备与临时镜像"
}
trap cleanup EXIT

say() { printf '\n\033[1m===== %s =====\033[0m\n' "$1"; }

# ---------- 0. 前置检查：别把用户现有卷搞坏 ----------
say "0) 前置检查"
if [ -e "$MOUNT" ]; then
  echo "⚠️  $MOUNT 已存在，拒绝继续（换个没占用的编号）"
  exit 1
fi
if [ -L "/dev/mapper/trim_fn_probe" ] || lvs 2>/dev/null | grep -q fn_probe; then
  echo "⚠️  已存在 fn_probe 残留，先手工清掉"; exit 1
fi

# ---------- 1. 先看飞牛 mount 表长什么样 ----------
say "1) 飞牛 mount 表结构（关键：uuid 字段到底存什么）"
sudo -u postgres psql -d trim -P pager=off -c "\d mount" 2>&1

say "1b) 现有记录（看系统卷那条 uuid 的实际格式）"
sudo -u postgres psql -d trim -P pager=off -c "SELECT * FROM mount ORDER BY id;" 2>&1
echo
echo "↑ 重点看 uuid 列：是 'trim_xxxx-0' 这种设备名，还是别的？"
echo "  另外确认 id=$ID 没被占用"

say "1c) 飞牛存储服务的主程序在哪（后面可以 strings 搜 trim_ 前缀）"
systemctl show -p ExecStart -p FragmentPath trim_main.service 2>/dev/null
ls -d /usr/trim* /opt/trim* 2>/dev/null

# ---------- 2. 造一个 2G 的假 btrfs 盘 ----------
say "2) 造测试盘（loop 设备，2G，与你的真实数据无关）"
truncate -s 2G "$IMG"
LO=$(losetup -f --show "$IMG")
echo "loop 设备: $LO"
mkfs.btrfs -f "$LO" >/dev/null 2>&1
mkdir -p "$TMPDIR_MNT"
mount "$LO" "$TMPDIR_MNT"
btrfs subvolume create "$TMPDIR_MNT/@syno" >/dev/null
SYNO_ID=$(btrfs subvolume list "$TMPDIR_MNT" | awk '/@syno$/{print $2}')
echo "@syno 子卷 ID: $SYNO_ID"
btrfs subvolume set-default "$SYNO_ID" "$TMPDIR_MNT" >/dev/null
umount "$TMPDIR_MNT"
echo "测试盘就绪（已建 @syno 并设为默认子卷，模拟群晖布局）"

# ---------- 3. 实验 A：裸设备直接注册 ----------
say "3) 实验 A —— 裸设备（无 LVM）注册"
mkdir -p "$MOUNT"
if ! mount -o rw,noatime,trimacl,space_cache=v2 "$LO" "$MOUNT"; then
  echo "❌ 挂载失败"; exit 1
fi
echo "已挂载 $LO -> $MOUNT"
df -h "$MOUNT" | tail -1

sudo -u postgres psql -d trim -P pager=off -c \
  "INSERT INTO mount (id,uuid,comment,sync_speed_min,sync_speed_max,stripe_cache_size,warn_frsize,grow,state,pool_type)
   VALUES ($ID,'$LO','',-1,-1,4096,10,'',0,0) ON CONFLICT DO NOTHING;" 2>&1

echo
echo "已写入 mount 表。现在请："
echo "  1) sudo systemctl restart trim_main.service"
echo "  2) 打开飞牛 Web → 存储空间管理，看有没有出现 $MOUNT"
read -r -p "看完后按回车继续（会清理并进入实验 B）..."

# 不管结果如何，先撤掉 A
sudo -u postgres psql -d trim -P pager=off -c "DELETE FROM mount WHERE id=$ID;" >/dev/null 2>&1
umount "$MOUNT" 2>/dev/null

# ---------- 4. 实验 B：套一层 LVM 再注册（对照组） ----------
say "4) 实验 B —— 套 LVM（trim_ 命名）注册，作为对照"
NEWUUID=$(cat /proc/sys/kernel/random/uuid)
VG="trim_$(echo "$NEWUUID" | tr '-' '_')"
pvcreate -ff -y "$LO" >/dev/null 2>&1 || { echo "pvcreate 失败"; exit 1; }
vgcreate "$VG" "$LO" >/dev/null 2>&1 || { echo "vgcreate 失败"; exit 1; }
# 注意：这里 mkfs 会重建文件系统，仅用于验证飞牛认不认这个命名，不代表数据能保住
lvcreate -l 100%FREE -n 0 -y "$VG" >/dev/null 2>&1 || { echo "lvcreate 失败"; exit 1; }
mkfs.btrfs -f "/dev/mapper/${VG}-0" >/dev/null 2>&1
mount "/dev/mapper/${VG}-0" "$TMPDIR_MNT"
btrfs subvolume create "$TMPDIR_MNT/@syno" >/dev/null
SID=$(btrfs subvolume list "$TMPDIR_MNT" | awk '/@syno$/{print $2}')
btrfs subvolume set-default "$SID" "$TMPDIR_MNT" >/dev/null
umount "$TMPDIR_MNT"

mount -o rw,noatime,trimacl,space_cache=v2 "/dev/mapper/${VG}-0" "$MOUNT"
echo "已挂载 /dev/mapper/${VG}-0 -> $MOUNT"
sudo -u postgres psql -d trim -P pager=off -c \
  "INSERT INTO mount (id,uuid,comment,sync_speed_min,sync_speed_max,stripe_cache_size,warn_frsize,grow,state,pool_type)
   VALUES ($ID,'${VG}-0','',-1,-1,4096,10,'',0,0) ON CONFLICT DO NOTHING;" 2>&1

echo
echo "已写入 mount 表。现在请："
echo "  1) sudo systemctl restart trim_main.service"
echo "  2) 打开飞牛 Web → 存储空间管理，看有没有出现 $MOUNT"
read -r -p "看完后按回车，脚本会完整清理..."

# ---------- 5. 结论提示 ----------
say "5) 结果判读"
cat <<'TIP'
  A 认、B 也认   → 裸设备可行，md2/md3 可以做零拷贝接管（v0.6 加这条路）
  A 不认、B 认   → 飞牛强依赖 trim_ 卷组命名，md2/md3 无法零拷贝接管
                   兜底：只读挂到已有存储空间的子目录里访问数据
  A 认、B 不认   → 不太可能，检查 B 的实验步骤
  两个都不认     → 说明 mount 表还有我们没填的必填字段，回到第 1 步看表结构
TIP

# 清理 LVM（trap 之后还要再清一次，顺序要对）
umount "$MOUNT" 2>/dev/null
sudo -u postgres psql -d trim -P pager=off -c "DELETE FROM mount WHERE id=$ID;" >/dev/null 2>&1
lvchange -an "/dev/mapper/${VG}-0" 2>/dev/null
vgremove -f "$VG" >/dev/null 2>&1
pvremove -ff -y "$LO" >/dev/null 2>&1
echo "实验 B 的 LVM 已清理"
