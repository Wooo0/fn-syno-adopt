#!/bin/bash
# 群晖卷「重建子卷」工具（在**旧内核**里跑：DSM7 本机，或 4.15.0-20 / 4.4 救援 VM）
#
# 为什么需要它
# ------------
# 群晖 DSM7 给 inode 打了私有标志 0x40000000。飞牛（内核 6.x）的 tree-checker
# 读到就判 corrupt leaf，整个子卷挂不上：
#     corrupt leaf: root=257 ... ino=8512, unknown incompat flags detected: 0x40000000
# 重建 = 把这棵子树的内容重新写进一棵**全新子卷**，新 inode 由当前内核生成，
# 天然不带私有标志。数据通过 reflink 克隆，正常情况下不占额外空间、不搬字节。
#
# 这个盘的布局（真机 2026-10-31 摸清）
# ------------------------------------
#   257  @syno        DSM 卷根：@database / @tmp / @eaDir / 存档(空挂载点) …
#   259  数据子卷      「存档」的真实内容（影视库），DSM 用 subvolid 挂到 @syno/存档
# 两者在文件系统里都**没有目录名**（DSM 靠 root tree 里的 "default" 指认），
# 所以本工具全程按 ID 挂载，不依赖子卷名。
#
# 用法
# ----
#   sudo bash rebuild-subvol.sh plan  /dev/mapper/vg1001-lv --data 259 --at 存档
#   sudo bash rebuild-subvol.sh run   /dev/mapper/vg1001-lv --data 259 --at 存档
#   sudo bash rebuild-subvol.sh verify /dev/mapper/vg1001-lv
#   sudo bash rebuild-subvol.sh cleanup /dev/mapper/vg1001-lv --yes
#
#   可选：--syno 257（默认 257，即群晖卷根）
#         --allow-low-space   空间不够时仍继续（reflink 生效时其实不需要空间）
#
# 各模式做什么
# ------------
#   plan    只读：挂 ro、统计文件数与占用、看剩余空间、列子卷、给出空间结论
#   run     建 @syno_new，把「@syno 的全部内容」+「数据子卷内容」拷进去（reflink 优先）
#           —— 全程不删、不改旧子卷；失败就退出，旧数据原样在那儿
#   verify  对账：源 vs 新树的文件数/占用
#   cleanup set-default 到新树 → 把新树改名 @syno（旧树从此无引用，留作回退保险）
#
# 安全约定
# --------
#   - 只有 run/cleanup 会写盘，且都要求显式子命令；plan 是纯只读
#   - cleanup 必须先有 run 成功留下的标记文件
#   - 任何一步失败立刻退出，绝不"继续往下删"

set -uo pipefail

FS_MNT=/mnt/syno-fs          # subvolid=5（文件系统根，所有子卷的父）
OLD_MNT=/mnt/syno-old        # 旧 @syno（卷根）
DATA_MNT=/mnt/syno-data      # 数据子卷
NEW_NAME=@syno_new           # 新树临时名（建成后再改名为 @syno）
MARKER_DIR=/var/tmp

MODE="${1:-}"
DEV="${2:-}"
[ -n "$MODE" ] && [ -n "$DEV" ] || {
  echo "用法: sudo bash $0 {plan|run|verify|cleanup} /dev/mapper/vgXXXX-lv [--data N] [--at NAME] [--syno N] [--yes] [--allow-low-space]"
  echo "      cleanup 加 --keep-old 则旧树改名为 @syno_dsm_orig 保留（回退用，几乎不占空间）"
  exit 2
}

SYNO_ID=257
DATA_ID=""
AT_NAME=""
ASSUME_YES=0
ALLOW_LOW_SPACE=0
KEEP_OLD=0
KEEP_NAME=@syno_dsm_orig    # cleanup --keep-old 时旧树改成的名字
shift 2
while [ "$#" -gt 0 ]; do
  case "$1" in
    --data) DATA_ID="${2:-}"; shift 2 ;;
    --at)   AT_NAME="${2:-}"; shift 2 ;;
    --syno) SYNO_ID="${2:-}"; shift 2 ;;
    --yes)  ASSUME_YES=1; shift ;;
    --allow-low-space) ALLOW_LOW_SPACE=1; shift ;;
    --keep-old) KEEP_OLD=1; shift ;;
    --keep-name) KEEP_NAME="${2:-}"; shift 2 ;;
    *) echo "未知参数: $1"; exit 2 ;;
  esac
done

die() { echo "✗ $*" >&2; exit 1; }
ok()  { echo "✓ $*"; }
inf() { echo "· $*"; }
hr()  { echo "------------------------------------------------------------"; }

[ "$(id -u)" = 0 ] || die "请用 root 运行"
[ -b "$DEV" ] || die "设备不存在: $DEV"

FS_UUID=$(blkid -s UUID -o value "$DEV" 2>/dev/null)
MARKER="$MARKER_DIR/rebuild-subvol-${FS_UUID}.done"

mounted_at() { awk -v d="$1" '$2==d {print $1; exit}' /proc/mounts; }
mount_opts_at() { awk -v d="$1" '$2==d {print $4; exit}' /proc/mounts; }

ensure_dir() { [ -d "$1" ] || mkdir -p "$1"; }

# ---------- 挂载/卸载 ----------
cleanup_mounts() {
  for m in "$DATA_MNT" "$OLD_MNT" "$FS_MNT"; do
    [ -n "${m:-}" ] || continue
    if [ -n "$(mounted_at "$m")" ]; then
      umount "$m" 2>/dev/null || umount -l "$m" 2>/dev/null || true
    fi
    rmdir "$m" 2>/dev/null || true
  done
}
trap 'cleanup_mounts' EXIT

mount_fs() {   # $1 = ro|rw
  ensure_dir "$FS_MNT"
  if [ -n "$(mounted_at "$FS_MNT")" ]; then
    [ "$(mounted_at "$FS_MNT")" = "$DEV" ] \
      || die "$FS_MNT 已被 $(mounted_at "$FS_MNT") 占用（不是 $DEV）。先手工 umount $FS_MNT"
    # 已经挂上了：run/cleanup 需要写权限时要重挂，别傻乎乎再 mount 一次
    if [ "$1" = "rw" ]; then
      case ",$(mount_opts_at "$FS_MNT")," in
        *,rw,*) : ;;
        *) mount -o remount,rw "$FS_MNT" || die "已挂载但重挂为读写失败: $FS_MNT" ;;
      esac
    fi
  else
    mount -o "$1,subvolid=5" "$DEV" "$FS_MNT" || die "挂载文件系统根失败。先在救援 VM 里确认阵列/卷组已就绪：
    mdadm --assemble --scan
    vgchange -ay
    ls -l $DEV
  再确认内核是旧内核（uname -r 应 < 5.4，推荐 4.15.0-20 或 4.4）"
  fi
  # 检查它确实可写（ro 模式跳过）
  if [ "$1" = "rw" ]; then
    touch "$FS_MNT/.write-test" 2>/dev/null || die "文件系统挂不上写模式——检查是否被别处挂载"
    rm -f "$FS_MNT/.write-test"
  fi
}

mount_sub() {  # $1=subvolid $2=挂载点 $3=ro|rw
  ensure_dir "$2"
  local curdev cur
  curdev=$(mounted_at "$2"); cur=$(mount_opts_at "$2")
  if [ -n "$curdev" ]; then
    # 绝不能"看到有挂载就用"：可能是另一块盘的残留，认错盘就是灾难
    [ "$curdev" = "$DEV" ] || die "$2 已被 $curdev 占用（不是 $DEV）。先手工 umount $2"
    case ",$cur," in
      *",subvolid=$1,"*) : ;;
      *) die "$2 上挂的是别的子卷（$cur），不是 subvolid=$1。先手工 umount $2" ;;
    esac
  else
    mount -o "$3,subvolid=$1" "$DEV" "$2" || die "挂载子卷 $1 失败（旧内核读不了它？）"
  fi
}

# 子卷对普通挂载是「透明」的：du 默认会穿过子卷边界，把嵌套的数据子卷算进 @syno，
# 于是出现「@syno 3.41T + 存档 3.41T = 6.8T」的假账；而 find -xdev 又会在边界停下。
# 两种口径必须分开，否则空间判断全错。
du_own()  { du -sx --block-size=1 "$1" 2>/dev/null | awk '{print $1}'; }   # 只算本子卷自己的
du_deep() { du -s  --block-size=1 "$1" 2>/dev/null | awk '{print $1}'; }   # 连嵌套子卷一起算
du_bytes() { du_own "$1"; }                                                # 源一律用 own 口径
count_own()  { find "$1" -xdev 2>/dev/null | wc -l; }
count_deep() { find "$1" 2>/dev/null | wc -l; }
count_inodes() { count_own "$1"; }
free_bytes() { df -B1 --output=avail "$1" 2>/dev/null | tail -1 | tr -d ' ' || true; }
free_bytes_safe() { local v; v=$(free_bytes "$1"); echo "${v:-0}"; }
human() { awk -v b="$1" 'BEGIN{ split("B KiB MiB GiB TiB",u," "); i=1; while (b>=1024 && i<5){b/=1024;i++} printf "%.2f %s", b, u[i] }'; }

kernel_ver() { uname -r; }
is_old_kernel() {
  # 5.4 起 tree-checker 才校验 inode flags；这里只提示，不拦（DSM7 自己内核 4.4）
  local maj min
  maj=$(uname -r | cut -d. -f1); min=$(uname -r | cut -d. -f2)
  [ "$maj" -lt 5 ] && return 0
  [ "$maj" -eq 5 ] && [ "$min" -lt 4 ] && return 0
  return 1
}

# ---------- 检查挂载里是否有 @syno / 数据子卷 ----------
subvol_exists() { btrfs subvolume list -u "$FS_MNT" 2>/dev/null | awk '{print $2}' | grep -qx "$1"; }

# ============================ plan ============================
do_plan() {
  hr; echo "【演练模式：只读，不改盘上任何东西】"; hr
  inf "设备      : $DEV"
  inf "当前内核  : $(kernel_ver)"
  if is_old_kernel; then ok "内核 < 5.4，能读群晖私有标志"; else
    echo "⚠ 内核 $(kernel_ver) 可能仍会校验 inode 私有标志（5.4+ 加固）。"
    echo "  若后面 mount -o subvolid=$SYNO_ID 失败，说明必须换 4.15.0-20 / 4.4 内核。"
  fi
  [ -n "$DATA_ID" ] || die "plan 需要 --data <数据子卷ID>"

  mount_fs ro
  ok "文件系统根已只读挂载到 $FS_MNT"
  echo
  echo "现有子卷（$DEV）："
  btrfs subvolume list -u "$FS_MNT" 2>&1 | sed 's/^/  /'
  echo
  inf "接下来按 ID 挂两个子卷（不依赖子卷名，DSM 的 @syno 在文件系统里没有目录名）"
  mount_sub "$SYNO_ID" "$OLD_MNT" ro && ok "子卷 $SYNO_ID(@syno) 已挂到 $OLD_MNT"
  mount_sub "$DATA_ID" "$DATA_MNT" ro && ok "子卷 $DATA_ID(数据) 已挂到 $DATA_MNT"
  echo
  echo "顶层条目："
  echo "  @syno($SYNO_ID):  $(ls -A "$OLD_MNT" 2>/dev/null | head -12 | tr '\n' ' ')"
  echo "  数据($DATA_ID)  :  $(ls -A "$DATA_MNT" 2>/dev/null | head -12 | tr '\n' ' ')"
  echo

  local old_b data_b old_n data_n need free
  old_b=$(du_bytes "$OLD_MNT"); data_b=$(du_bytes "$DATA_MNT")
  old_n=$(count_inodes "$OLD_MNT"); data_n=$(count_inodes "$DATA_MNT")
  need=$(( old_b + data_b )); free=$(free_bytes_safe "$FS_MNT")

  echo "规模："
  printf '  @syno(%s)   : %8s 个文件   %10s\n' "$SYNO_ID" "$old_n" "$(human "${old_b:-0}")"
  printf '  数据(%s)   : %8s 个文件   %10s\n' "$DATA_ID" "$data_n" "$(human "${data_b:-0}")"
  printf '  合计待重建 : %8s 个文件   %10s\n' "$((old_n + data_n))" "$(human "$need")"
  echo
  printf '  文件系统剩余空间: %s\n' "$(human "$free")"
  echo
  if [ "$free" -ge "$need" ]; then
    ok "空间足够：即使 reflink 全部退化成真拷贝，也放得下（需 $(human "$need")，有 $(human "$free")）"
  else
    echo "⚠ 空间不足：真拷贝需要 $(human "$need")，只剩 $(human "$free")。"
    echo "  - 若 reflink 生效（同 FS 内克隆只花元数据），不需要这些空间，run 可以正常完成；"
    echo "  - 若某个目录因 nodatacow 混排退化成真拷贝，可能在半路撑爆。"
    echo "  - 想强行尝试：run 时加 --allow-low-space（中途空间不足会立刻中止，旧数据不动）。"
  fi
  echo
  hr
  echo "下一步：sudo bash $0 run $DEV --data $DATA_ID --at <数据子卷对应的挂载点名>"
  [ -n "$AT_NAME" ] && echo "        （你给的挂载点是：$AT_NAME）"
  hr
}

# ============================ run ============================
mirror_dirs() {  # $1=源 $2=目标 [$3=整棵跳过哪个相对路径] —— 先镜像目录结构和属性，C 属性（nodatacow）必须跟源一致
  local src=$1 dst=$2 skip=${3:-} rel d
  ( cd "$src" || exit 1
    find . -type d -print0 | while IFS= read -r -d '' d; do
      rel=${d#./}
      if [ -z "$rel" ]; then continue; fi
      # 嵌套子卷要单独建成子卷。注意必须连它的子树一起跳过——只跳这一层的话，
      # 下面 ensure_dir 的 mkdir -p 会把父目录当普通目录又建回来。
      if [ -n "$skip" ]; then
        case "$rel" in "$skip"|"$skip"/*) continue ;; esac
      fi
      ensure_dir "$dst/$rel"
      chmod --reference="$src/$rel" "$dst/$rel" 2>/dev/null
      chown --reference="$src/$rel" "$dst/$rel" 2>/dev/null
      touch -r "$src/$rel" "$dst/$rel" 2>/dev/null
      # 群晖大量文件是 nodatacow：目标目录必须同样带 C，FICLONE 才能成功
      if lsattr -d "$src/$rel" 2>/dev/null | awk '{print $1}' | grep -q C; then
        chattr +C "$dst/$rel" 2>/dev/null
      fi
    done )
}

do_run() {
  [ -n "$DATA_ID" ] || die "run 需要 --data <数据子卷ID>"
  [ -n "$AT_NAME" ] || die "run 需要 --at <挂载点名>（数据子卷挂在 @syno 下的那个目录，如 存档 / Downloads）"

  hr; echo "【执行重建】新树名：$NEW_NAME（完成后再改名 @syno）"; hr
  mount_fs rw
  ok "文件系统根已读写挂载：$FS_MNT"
  mount_sub "$SYNO_ID" "$OLD_MNT" ro && ok "旧 @syno($SYNO_ID) 已只读挂载"
  mount_sub "$DATA_ID" "$DATA_MNT" ro && ok "数据子卷($DATA_ID) 已只读挂载"
  echo

  # ⚠ FICLONE 不能跨挂载点：源在 /mnt/syno-data、目标在 /mnt/syno-fs 时，
  # 内核在 vfs_clone_file_range 直接返回 EXDEV（"Invalid cross-device link"），
  # cp 只会静默退化成真拷贝（--reflink=always 才会报错）。
  # 所以：OLD_MNT / DATA_MNT 这两次挂载只用来「量体积」，拷贝一律走 $FS_MNT 下的路径，
  # 让源和目标待在同一个挂载里。（子卷对普通挂载透明，穿过边界可达。）
  local SRC_OLD SRC_DATA
  SRC_OLD=$(btrfs subvolume list "$FS_MNT" | awk -v i="$SYNO_ID" '$2==i {print $NF}')
  [ -n "$SRC_OLD" ] || die "旧 @syno($SYNO_ID) 在文件系统里没有目录名，无法在同一挂载内 reflink"
  SRC_OLD="$FS_MNT/$SRC_OLD"
  SRC_DATA="$SRC_OLD/$AT_NAME"
  [ -d "$SRC_DATA" ] || die "在 $SRC_OLD 下找不到 $AT_NAME"
  ok "拷贝源与目标同挂载：$SRC_OLD"

  local old_b data_b need free free0
  old_b=$(du_bytes "$OLD_MNT"); data_b=$(du_bytes "$DATA_MNT")
  need=$(( ${old_b:-0} + ${data_b:-0} ))
  free=$(free_bytes_safe "$FS_MNT"); free0=$free
  inf "待重建 $(human "$need")，剩余 $(human "$free")"
  if [ "$free" -lt "$need" ] && [ "$ALLOW_LOW_SPACE" -ne 1 ]; then
    die "剩余空间小于数据量，可能在中途撑爆。确认要试就加 --allow-low-space（中途会监视空间，不足即中止）"
  fi
  echo

  NEW_DIR="$FS_MNT/$NEW_NAME"
  if [ -e "$NEW_DIR" ]; then
    inf "已存在 $NEW_NAME，复用（不会清空；如果是上次残留请先人工确认）"
  else
    btrfs subvolume create "$NEW_DIR" || die "创建 $NEW_NAME 失败"
    ok "已创建子卷 $NEW_NAME"
  fi

  echo
  echo "⓪ reflink 预检（决定这 $(human "$need") 能不能零空间完成）…"
  local probe probe_dir ex
  probe=$(find "$SRC_DATA" -type f -printf '%s\t%p\n' 2>/dev/null | sort -rn | head -1 | cut -f2-)
  if [ -z "$probe" ]; then
    inf "  数据子卷里没有文件，跳过预检"
  else
    probe_dir="$NEW_DIR/.reflink-probe"
    ensure_dir "$probe_dir"
    chattr +C "$probe_dir" 2>/dev/null || true
    if cp --reflink=always -- "$probe" "$probe_dir/" 2>/tmp/rf.err; then
      ex=$(btrfs filesystem du -s "$probe_dir" 2>/dev/null | awk 'NR==2{print $2}')
      ok "  可 reflink：拿最大的文件硬试成功，独占字节 = ${ex:-未知}"
      inf "  （独占 0.00B 才是真 clone；若等于文件大小说明退化了）"
      rm -rf "$probe_dir"
    else
      rm -rf "$probe_dir"
      die "reflink 不可用：$(head -1 /tmp/rf.err)
    本盘被迫真拷贝，需要 $(human "$need")，只剩 $(human "$free") —— 会撑爆，已中止。
    旧数据一字节未动。改走「外拷到别的盘」或内核补丁路线。"
    fi
  fi

  echo
  echo "① 镜像 @syno 的目录结构（含 C 属性）…"
  mirror_dirs "$SRC_OLD" "$NEW_DIR" "$AT_NAME"
  ok "目录结构完成"

  echo "② 拷贝 @syno 自己的内容（跳过嵌套子卷，否则会被拷两遍）…"
  local t0 t1 e skipped=0
  t0=$(date +%s)
  # 子卷根目录的 inode 恒为 256，用它识别「这是个嵌套子卷」，不要递归进去
  while IFS= read -r e; do
    [ -n "$e" ] || continue
    if [ "$(stat -c '%i' "$SRC_OLD/$e" 2>/dev/null)" = "256" ]; then
      inf "  跳过嵌套子卷: $e（留给第③步单独处理）"
      skipped=$((skipped+1)); continue
    fi
    cp -a --reflink=auto "$SRC_OLD/$e" "$NEW_DIR/" \
      || die "拷贝 @syno/$e 失败（已中止，旧数据未动）"
  done < <(ls -A "$SRC_OLD")
  ok "完成，用时 $(( $(date +%s) - t0 ))s（跳过 $skipped 个嵌套子卷）"

  echo
  echo "③ 把数据子卷($DATA_ID) 建成 $AT_NAME/ 的嵌套子卷，内容 reflink 进去 …"
  # 关键：这里必须建「子卷」而不是普通目录，否则共享文件夹的子卷身份就丢了
  if [ "$(stat -c '%i' "$NEW_DIR/$AT_NAME" 2>/dev/null)" = "256" ]; then
    inf "  $AT_NAME 已经是子卷（上次残留？），直接复用"
  else
    # ①阶段如果已经把它建成了空目录树，先清掉；只要里面有一个文件就绝不动手
    if [ -e "$NEW_DIR/$AT_NAME" ]; then
      if [ -z "$(find "$NEW_DIR/$AT_NAME" -type f 2>/dev/null | head -1)" ]; then
        find "$NEW_DIR/$AT_NAME" -depth -type d -empty -delete
        inf "  清掉①阶段残留的空目录树，改建子卷"
      fi
      [ -e "$NEW_DIR/$AT_NAME" ] \
        && die "$NEW_DIR/$AT_NAME 已存在且里面有文件、又不是子卷 —— 先人工确认再跑"
    fi
    btrfs subvolume create "$NEW_DIR/$AT_NAME" || die "创建嵌套子卷 $AT_NAME 失败（旧数据未动）"
    ok "  已创建嵌套子卷 $AT_NAME（保留共享文件夹的子卷身份）"
    if lsattr -d "$SRC_DATA" 2>/dev/null | awk '{print $1}' | grep -q C; then
      chattr +C "$NEW_DIR/$AT_NAME" && ok "  已镜像数据子卷根目录的 C 属性（nodatacow）"
    fi
  fi
  mirror_dirs "$SRC_DATA" "$NEW_DIR/$AT_NAME"
  t0=$(date +%s)
  cp -a --reflink=auto "$SRC_DATA"/. "$NEW_DIR/$AT_NAME"/ || die "拷贝数据子卷失败（已中止，旧数据未动）"
  ok "完成，用时 $(( $(date +%s) - t0 ))s"

  echo
  local new_b new_n used_up
  new_b=$(du_deep "$NEW_DIR"); new_n=$(count_deep "$NEW_DIR")
  free=$(free_bytes_safe "$FS_MNT")
  used_up=$(( free0 - free ))
  echo "结果："
  printf '  源合计 : %8s 个文件   %10s\n' "$(( $(count_inodes "$OLD_MNT") + $(count_inodes "$DATA_MNT") ))" "$(human "$need")"
  printf '  新树   : %8s 个文件   %10s\n' "$new_n" "$(human "$new_b")"
  printf '  剩余空间: %s（开始前 %s，实际消耗 %s）\n' "$(human "$free")" "$(human "$free0")" "$(human "$used_up")"
  echo
  if [ "$new_n" -lt $(( $(count_inodes "$OLD_MNT") + $(count_inodes "$DATA_MNT") - 1 )) ]; then
    echo "⚠ 新树文件数偏少——可能有文件没拷到。请先跑 verify 对账，别急着 cleanup。"
  fi
  # 真 reflink 只花元数据（几万文件约几百 MB）。消耗超过 20 GiB 说明大量文件退化成了真拷
  if [ "$used_up" -gt $(( 20 * 1024 * 1024 * 1024 )) ]; then
    echo "⚠ 空间实际消耗 $(human "$used_up")，远超元数据量——相当一部分文件退化成了真拷贝。"
    echo "  虽然这次没撑爆，但要知道：这些文件的 extents 是独占的。"
  else
    ok "空间消耗仅 $(human "$used_up")，reflink 基本全程生效（没有真拷数据）"
  fi

  printf '%s\n' "dev=$DEV uuid=$FS_UUID syno=$SYNO_ID data=$DATA_ID at=$AT_NAME new=$NEW_NAME time=$(date -Is)" > "$MARKER"
  ok "已写标记：$MARKER"
  echo
  hr
  echo "下一步（先对账，再切默认）："
  echo "  sudo bash $0 verify $DEV --data $DATA_ID --at $AT_NAME"
  echo "  sudo bash $0 cleanup $DEV --data $DATA_ID --yes"
  hr
}

# ============================ verify ============================
do_verify() {
  [ -n "$DATA_ID" ] || die "verify 需要 --data <数据子卷ID>"
  [ -n "$AT_NAME" ] || die "verify 需要 --at <挂载点名>"
  hr; echo "【对账】"; hr
  mount_fs ro
  mount_sub "$SYNO_ID" "$OLD_MNT" ro
  mount_sub "$DATA_ID" "$DATA_MNT" ro
  NEW_DIR="$FS_MNT/$NEW_NAME"
  [ -d "$NEW_DIR" ] || die "找不到新树 $NEW_DIR"

  local o_b o_n d_b d_n n_b n_n
  o_b=$(du_bytes "$OLD_MNT"); o_n=$(count_inodes "$OLD_MNT")
  d_b=$(du_bytes "$DATA_MNT"); d_n=$(count_inodes "$DATA_MNT")
  n_b=$(du_deep "$NEW_DIR");  n_n=$(count_deep "$NEW_DIR")

  printf '  @syno(%s)   : %8s 项  %10s\n' "$SYNO_ID" "$o_n" "$(human "${o_b:-0}")"
  printf '  数据(%s)    : %8s 项  %10s\n' "$DATA_ID" "$d_n" "$(human "${d_b:-0}")"
  printf '  新树        : %8s 项  %10s\n' "$n_n" "$(human "${n_b:-0}")"
  printf '  期望≈       : %8s 项  %10s\n' \
         "$(( o_n + d_n - 1 ))" "$(human $(( ${o_b:-0} + ${d_b:-0} )))"
  echo "      （= 两个源相加；$AT_NAME 这个目录在两个源里各出现一次，故重复计 1）"
  echo
  # 逐层抽样比对（不做全量 diff：3.6T 全比要跑很久）
  echo "抽样比对（顶层条目 + 各自最大的 3 个文件的大小）："
  local miss=0
  while IFS= read -r e; do
    [ "$e" = "$AT_NAME" ] && continue
    [ -e "$NEW_DIR/$e" ] || { echo "  ✗ 新树缺: $e"; miss=$((miss+1)); }
  done < <(ls -A "$OLD_MNT")
  while IFS= read -r e; do
    [ -e "$NEW_DIR/$AT_NAME/$e" ] || { echo "  ✗ 新树缺: $AT_NAME/$e"; miss=$((miss+1)); }
  done < <(ls -A "$DATA_MNT")
  [ "$miss" -eq 0 ] && ok "两边顶层条目全部到位" || echo "⚠ 有 $miss 个顶层条目缺失，先别 cleanup"
  echo
  if [ "$n_n" -lt "$(( o_n + d_n - 1 ))" ]; then
    echo "⚠ 项数偏少：新树 $n_n < 期望 $(( o_n + d_n - 1 ))。先查上面的缺失条目，别急着 cleanup。"
  else
    ok "项数不低于期望"
  fi
  echo
  hr
  echo "对账通过后再执行： sudo bash $0 cleanup $DEV --data $DATA_ID --yes"
  hr
}

# ============================ cleanup ============================
do_cleanup() {
  [ -f "$MARKER" ] || die "没找到 run 成功留下的标记 $MARKER（先跑 run）"
  [ "$ASSUME_YES" -eq 1 ] || die "cleanup 会切默认子卷并删除旧引用，必须加 --yes 确认"
  hr; echo "【切换默认子卷 + 改名】"; hr
  mount_fs rw
  NEW_DIR="$FS_MNT/$NEW_NAME"
  [ -d "$NEW_DIR" ] || die "找不到新树 $NEW_DIR"
  umount "$OLD_MNT" 2>/dev/null; umount "$DATA_MNT" 2>/dev/null

  local new_id
  new_id=$(btrfs subvolume list "$FS_MNT" | awk -v p="$NEW_NAME" '$NF==p {print $2; exit}')
  [ -n "$new_id" ] || die "读不到 $NEW_NAME 的子卷 ID"

  btrfs subvolume set-default "$new_id" "$FS_MNT" || die "set-default 失败"
  ok "默认子卷已切到 $NEW_NAME（id=$new_id）"

  if [ "$KEEP_OLD" -eq 1 ]; then
    # 不删旧树，改名留作回退。两棵树的数据 extent 全部 reflink 共享，
    # 留着只多几百 MB 元数据，却能一条 mv 退回原状——比删掉划算得多。
    local op
    op=$(btrfs subvolume list "$FS_MNT" | awk -v i="$SYNO_ID" '$2==i {print $NF}')
    if [ -n "$op" ] && [ -e "$FS_MNT/$op" ]; then
      [ "$op" = "$KEEP_NAME" ] && die "旧树已经叫 $KEEP_NAME 了，别重复改"
      [ -e "$FS_MNT/$KEEP_NAME" ] && die "$FS_MNT/$KEEP_NAME 已存在，先人工处理"
      mv "$FS_MNT/$op" "$FS_MNT/$KEEP_NAME" \
        || die "把旧树改名为 $KEEP_NAME 失败（旧数据仍在，未破坏）"
      ok "旧树已改名为 $KEEP_NAME（保留回退，几乎不占空间）"
    else
      inf "旧 @syno($SYNO_ID) 没有目录名，无法改名保留；它只会留成无引用旧树"
    fi
  else
    # 删除顺序有讲究：旧 @syno 里套着数据子卷，btrfs 不允许删除「还带着子卷」的子卷，
    # 必须先把内层（数据子卷）删掉，再删外层（旧 @syno）。按子卷 ID 降序即天然满足
    # （子卷创建时 ID 递增，子必然大于父）。
    local del_ids id path
    del_ids="$SYNO_ID"
    [ -n "$DATA_ID" ] && del_ids="$DATA_ID $SYNO_ID"
    for id in $(printf '%s\n' $del_ids | sort -rn); do
      path=$(btrfs subvolume list "$FS_MNT" | awk -v i="$id" '$2==i {print $NF}')
      if [ -n "$path" ] && [ -e "$FS_MNT/$path" ]; then
        if btrfs subvolume delete "$FS_MNT/$path"; then
          ok "已删除旧子卷 $path (id=$id)"
        else
          echo "⚠ 删除 $path (id=$id) 失败——若提示还带着子卷，说明内层没删掉，先查："
          echo "    btrfs subvolume list $FS_MNT"
        fi
      else
        inf "旧子卷 $id 没有目录名（无引用旧树），保留"
        inf "  —— 它只占 extent 引用、不额外占空间；确认接管正常后可忽略"
      fi
    done

    # ★ 这一步不能省：subvolume delete 是**异步**的。
    # 元数据上子卷立刻消失（subvolume list 里看不到了），但 extent 由 cleaner 线程
    # 在后台慢慢回收，根树里的 ROOT_ITEM 会先变成 refs=0 的 dead root。
    # 如果这时直接卸载，dead root 就留在盘上了——而飞牛内核在**读写**挂载时会先
    # 去拆 dead root：读它的叶子 → 撞上旧子卷里带群晖私有 inode 标志(0x40000000)
    # 的 inode → tree-checker 拒绝 → open_ctree 失败，对外只表现成
    # "mount: can't read superblock"（exit 32）。
    # 2026-10-31 vg1001 / vg1002 就是这么放回飞牛挂不上的：删完立刻退出，
    # trap 里的 umount 抢在 cleaner 前面。
    # 只读挂载不会碰 dead root，所以"ro 能挂、rw 挂不上"是这类问题的特征。
    inf "等待 cleaner 把旧子卷真正删完（subvolume sync，数据量大时可能要几分钟）…"
    if btrfs subvolume sync "$FS_MNT"; then
      ok "旧子卷已彻底删除，根树里不再有 dead root"
    else
      die "subvolume sync 未成功——**不要卸载**。卸载会把 dead root 留在盘上，
          放回飞牛后 rw 挂载会失败（can't read superblock）。先人工确认：
          btrfs subvolume list $FS_MNT"
    fi
  fi

  if [ -e "$FS_MNT/@syno" ]; then
    echo "⚠ 已存在名为 @syno 的目录项，未自动改名。请人工确认："
    btrfs subvolume list "$FS_MNT" | sed 's/^/  /'
  else
    mv "$NEW_DIR" "$FS_MNT/@syno" || die "改名失败（可手工：mv $NEW_DIR $FS_MNT/@syno）"
    ok "已改名为 @syno —— 飞牛接管时按这个名字找默认子卷"
  fi

  echo
  echo "最终子卷清单："
  btrfs subvolume list "$FS_MNT" | sed 's/^/  /' || true
  btrfs subvolume show "$FS_MNT/@syno" 2>/dev/null | head -6 | sed 's/^/  /'
  echo
  ok "完成。把盘接回飞牛，重新点「生成接管计划」。"
}

case "$MODE" in
  plan)    do_plan ;;
  run)     do_run ;;
  verify)  do_verify ;;
  cleanup) do_cleanup ;;
  *) echo "未知模式: $MODE（plan|run|verify|cleanup）"; exit 2 ;;
esac
