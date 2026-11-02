# 救援 VM 清除群晖私有标记 Runbook

> ## ⭐ 最终执行单（2026-10-29 摸底定稿，以此为准）
>
> 摸底结论：被标对象分两类——
> ① **系统垃圾**（md2 的 @syno 树：@database/core dump/@tmp 等 ~7000 个）→ 容忍内核里直接 `rm`；
> ② **真实媒体文件**（三个共享子卷里 ~5.5 万个，flag 低 4 位是 NODATASUM/NODATACOW/NOCOMPRESS，
>    即 DSM 关"数据校验和"的共享写文件时打的私有位）→ reflink 搬家。
> root 层快照已在群晖端删净（路线 0 完成），只剩 sda 的 @syno(256)/MediaData2(258) 两个 root item 待除。
>
> ### 练手盘 md3（只有 MediaData3 一个子卷要搬）
> ```bash
> # 救援 VM 内：组装 + RW 挂 FS tree
> mdadm --assemble --run /dev/md127 /dev/sdX3 && mount -t btrfs -o rw,subvolid=5 /dev/md127 /mnt/x
> cd /mnt/x/@syno        # 数据主卷（default）
> # 1) 旧子卷让位（改名不清标记，只腾名字）
> mv MediaData3 MediaData3_old
> # 2) 新建子卷 + reflink 复制（秒级~分钟级，不占空间，新 inode 保证干净）
> btrfs subvolume create MediaData3_new
> cp -a --reflink=always MediaData3_old/. MediaData3_new/
> # 3) 对账（必须全部一致才继续）
> find MediaData3_old -xdev | wc -l ; find MediaData3_new -xdev | wc -l
> du -s --block-size=1 MediaData3_old ; du -s --block-size=1 MediaData3_new
> # 4) 顶替原名 + 删旧（删除才会清掉 root item；此后 md3 树内零私有标记）
> mv MediaData3_new MediaData3
> btrfs subvolume delete MediaData3_old
> btrfs subvolume delete /mnt/x/@sharesnap 2>/dev/null   # 空容器，可选
> ```
>
> ### md2（@syno 里有系统垃圾 + MediaData1 要搬）
> ```bash
> mdadm --assemble --run /dev/md2 /dev/sdX3 && mount -t btrfs -o rw /dev/md2 /mnt/x   # 默认子卷=@syno
> cd /mnt/x
> # 1) 删系统垃圾（清单已确认全是 @database/@tmp/core dump 等，无用户数据）
> rm -rf @database @tmp @userpreference @optware @SynologyDriveShareSync @spool @eaDir
> rm -f *.core.gz
> # 2) MediaData1 搬家（同 md3 四步：改名→建新→reflink→对账→删旧）
> ```
>
> ### sda（最特殊：@syno(256) 和 MediaData2(258) 的 root item 都带标）
> ```bash
> mdadm --assemble --run /dev/md6 /dev/sdX3 && vgchange -ay
> mount -t btrfs -o rw,subvolid=5 /dev/mapper/vg1-volume_7 /mnt/x && cd /mnt/x
> # 1) 新建干净顶层卷，内部再造 MediaData2 子卷
> btrfs subvolume create @syno_clean
> btrfs subvolume create @syno_clean/MediaData2
> # 2) reflink 复制
> mount -t btrfs -o ro,subvolid=258 /dev/mapper/vg1-volume_7 /mnt/old
> cp -a --reflink=always /mnt/old/. @syno_clean/MediaData2/
> # 3) 对账后：切默认 → 删两个带标子卷 → 顶替
> btrfs subvolume set-default <@syno_clean的ID> /mnt/x
> btrfs subvolume delete @syno/MediaData2 && btrfs subvolume delete @syno
> mv @syno_clean @syno
> ```
>
> ### 通用红线（不变）
> - 飞牛 VM 关机、盘只挂救援 VM；`--reflink=always` 不许省；对账不过不删旧；
> - 不跑 btrfs check --repair；不在飞牛 UI 对源盘点格式化；
> - 搬完回飞牛 `mount -t btrfs -o ro /dev/mdX /mnt/check && ls` 终验。
>
> ### ⚠️ 关键坑：reflink 必须先给目标打 +C（nodatacow）
> 媒体文件源 inode 是 NODATASUM|NODATACOW（flag 0x…313），cp 新建的普通目标文件是
> datacow/datasum → btrfs 拒绝跨校验和属性 reflink（EINVAL），cp 会留下**同名空壳文件**
> （症状：find 条目数一致、du/apparent-size 接近 0、无 >1G 文件）。
> 正确顺序：
> ```bash
> cp -a --reflink=always 旧/. 新/ 2>/tmp/cp.err      # 第一遍建树（数据多失败，没关系）
> find 新 -exec chattr +C {} + 2>/dev/null           # 全部条目打 +C（合法上游 flag，无私有位）
> cp -a --reflink=always 旧/. 新/ 2>/tmp/cp2.err     # 重跑，这次 clone 放行
> # 三重对账：du -s、du -s --apparent-size、find|wc -l 三组一致才许删旧
> ```
>
> ### ⚠️ 关键坑 2：reflink 不能跨挂载点（sda 踩过）
> 单独 `mount -o ro,subvolid=258 /dev/xxx /mnt/old` 后从 /mnt/old 克隆到 /mnt/ro，
> 4.15 内核报 `Invalid cross-device link`（EXDEV）——FICLONE 要求 src/dst 同挂载点。
> **修法：被克隆子卷不单独挂，用路径访问**（如 /mnt/ro/@syno/MediaData2）。
> md3 顺就是因为在同一挂载点内走路径；md2 默认子卷平铺同理。
>
> ### 救援内核版本矩阵（实测 2026-10-29）
> | 内核 | 结果 |
> |---|---|
> | 4.15.0-213（ESM 后期构建） | ❌ inode flag 校验被 backport，读叶子块直接 corrupt leaf |
> | 4.4.0-210 | ❌ 能 RO 读（inode 不校验）但 **RW 挂载被拒**：FST 文件系统 RW 要 4.9+（`unsupported option features (3)` = compat_ro FREE_SPACE_TREE\|VALID） |
> | **4.15.0-20（18.04.0 发布版）** | ✅ **甜点区**：FST RW ✓、无 inode 校验 ✓。装系统自带的旧内核就在 /boot，零下载 |
>
> ### 长尾提示
> 私有标记是 DSM 在"数据校验和关闭"的共享里写文件时打的。盘若再回群晖写新文件，标记会再生；
> 在飞牛上写入则永远干净。TOSHIBA（volume6，docker/homes 所在）未列入本流程，等定归宿。

---

> 目标：不依赖任何内核补丁、不复制数据出盘，用 btrfs 原生操作把 DSM7 私有标记
> （root flag 0x?00000000 / inode flag 0x?0000000）从盘上清掉，让飞牛能原生挂载。

## 路线 0（最省事，先试）：回群晖删快照

被标 root item 几乎全是快照子卷。把盘装回群晖，**每个卷把快照全部删掉**
（DSM 存储管理器/共享文件夹快照，或 `syno` 命令行），DSM 自己的内核会把
这些快照的 root item 正规摘除，标记随之消失。

- 删之前：用 DSM 快照浏览器确认快照里没有"只存在于快照中"的文件要留；
- 删之后：盘拿回飞牛，**必须复跑 `inspect-syno-flags.sh` 验证**——
  社区有人删的是共享子卷而非快照，标记没清掉（github limitcool/synology-btrfs-kernel-patch
  的失败记录）。以脚本输出为准，别凭感觉。
- 已知边界：
  - md3：root 层只差 261 一个快照，删完大概率直接解决（257 inode 层还没采样，回飞牛补采）；
  - md2：root 层能净，但 257 的 inode 层有 ~1 万个被标文件，删快照解决不了，仍需搬家；
  - sda：**主子卷 256 自己带标**，删快照解决不了，需要搬家或单点清除；
  - TOSHIBA：429/430/431 三个快照删掉后 root 层即净，inode 层待采样。
- 路线 0 没清干净的盘，再走下面的救援 VM 流程。

---

> 原理：用**内核 < 5.4** 的救援 VM（没有那个校验）把盘挂成 RW——
> ① 正规 `btrfs subvolume delete` 删掉带标记的快照子卷（root tree 即净）；
> ② 主数据子卷内新建子卷 + `cp -a --reflink=always` 搬家（reflink 是同盘元数据
>    操作，秒级、不占空间、数据不出盘），新 inode 全部干净；删旧子卷后 inode 层也净。
>
> ⚠️ 红线（每次动手前重读一遍）：
> 1. 全程只允许挂载源盘到**救援 VM**，飞牛 VM 必须先关机、摘掉这些盘，禁止两边同时挂；
> 2. 永远不跑 `btrfs check --repair`、不在飞牛 UI 上对源盘点"创建存储空间/格式化"；
> 3. `cp` 必须带 `--reflink=always`（不能 reflink 时会直接报错，而不是静默变成全量复制）；
> 4. 旧子卷在核验（文件数 + du 对账）通过前**绝不删除**；
> 5. 每删一个子卷前，先确认里面没有独有数据（快照与主卷共享数据块，但要人工看一眼目录）。

---

## 0. 前置准备（KVM 宿主机上）

### 0.1 救援内核：必须是 4.4 系（已实测 4.15 不行）

- ⚠️ **实测结论（2026-10-29）**：Ubuntu 18.04.1 的 4.15.0-213 内核**同样校验 inode flag**
  （dmesg：`corrupt leaf: root=257 ... unknown flags detected: 0x40000000`）——
  Ubuntu 把上游 tree-checker 加固 backport 进了 4.15 后续 ESM 构建。
  "内核 < 5.4 即可"的说法不成立，唯一稳妥锚点是 **4.4 系（DSM7 自身内核）**。
- 推荐姿势（不重建 VM）：在任意 Ubuntu VM 里原地装 xenial 的 4.4 内核
  ```bash
  echo "deb http://archive.ubuntu.com/ubuntu xenial-updates main" > /etc/apt/sources.list.d/xenial.list
  apt update && cd /tmp
  apt download linux-image-4.4.0-2XX-generic linux-image-extra-4.4.0-2XX-generic
  dpkg -i linux-image-4.4.0-2XX-generic*.deb linux-image-extra-4.4.0-2XX-generic*.deb
  # btrfs.ko 在 linux-image-extra 包里，缺一不可！
  # GRUB_DEFAULT 设为 "Advanced options for Ubuntu>Ubuntu, with Linux 4.4.0-2XX-generic"
  ```
- 整机备选：Ubuntu 16.04.7 Server（4.4 原生）：
  mirrors.ustc.edu.cn/ubuntu-old-releases/releases/16.04.7/ubuntu-16.04.7-server-amd64.iso
- ⚠️ Debian 10 (4.19) 大概率同样中招（4.16+ 上游加的 inode 校验可能已 backport），别赌
- 备选：Debian 10 (buster, 4.19) 网络安装镜像
- 进系统后第一件事：`uname -r` 确认主版本号 < 5.4，否则换镜像别硬试

### 0.2 换盘方式：HBA + 主板 SATA 整卡直通切换（不逐盘分离）

7 块盘不在两个 VM 之间逐盘摘挂，而是**切换 PCI 直通的归属**：

1. **关闭飞牛 VM**（整卡切换必须断电交接，禁止两边同时持有）；
2. 救援 VM 配置里加上直通设备（PCI 地址以 `lspci` 为准）：
   ```bash
   # 宿主机上先确认 PCI 地址
   lspci -nn | grep -i -e sata -e sas
   # 例：HBA 卡 0000:04:00.0、主板 SATA 0000:00:17.0
   qm set 900 -hostpci0 0000:04:00.0
   qm set 900 -hostpci1 0000:00:17.0
   # 或不加配置，直接编辑飞牛/救援 VM 硬件 → PCI 设备改目标也行
   ```
3. 启动救援 VM，`lsblk` 核对 7 块盘都到了；
4. 干完活反过来切：救援 VM 关机 → 飞牛 VM 开机即恢复（飞牛的直通配置不用动，
   PVE 启动时会检查 PCI 占用，谁开机谁持有，天然防呆）。

> ⚠️ 主板 SATA 控制器整卡直通的前提：PVE 宿主机自己的系统盘**不在**这组 SATA 口上
> （飞牛 VM 一直这么跑，说明已满足，仅提醒）。
> 盘符会漂移：切换直通后救援 VM 里的 sdX 与飞牛里的不保证一致，
> 以下命令一律先 `lsblk` 现场按容量/型号确认，不要照抄本文的 sdX 名。

---

## 1. 救援 VM 内：组装并只读查看（第一步永远只读）

```bash
apt install -y mdadm lvm2 btrfs-progs   # live 环境则跳过

# 1.1 组装 md 阵列（单成员阵列也能组）
mdadm --assemble --run /dev/md127 /dev/sdX3      # sdd3 → 原 md3，现场按 lsblk 定
cat /proc/mdstat                                  # 确认 [U] 状态

# 1.2 有 LVM 层的盘才需要（md3 没有，sda 有）
vgchange -ay                                      # 激活全部卷组

# 1.3 只读挂 FS tree（subvolid=5 = 顶层，能看到全部子卷目录）
mkdir -p /mnt/ro
mount -t btrfs -o ro,subvolid=5 /dev/md127 /mnt/ro

# 1.4 列出全部子卷（ID + 路径），和摸底结果核对
btrfs subvolume list /mnt/ro

# 1.5 先看数据：主数据子卷通常是 "default" 指向的那个（md3 = 257）
ls -la /mnt/ro/default/          # 核对：这是不是你的数据
du -sh  /mnt/ro/default/ 2>/dev/null &   # 大卷较慢，后台跑
```

✅ 检查点：能看到数据、`subvolume list` 与摸底一致 → 进入第 2 步。
❌ 如果快照目录里有主卷没有的独有文件（需要保留的内容），先 `cp` 出来再继续。

---

## 2. 练手盘 md3：完整清理流程（差异最小的盘）

md3 的被标对象只有一个：**261**（快照链 257→260→261）。主数据在 257。

```bash
# 2.1 重新以 RW 挂载
umount /mnt/ro
mount -t btrfs -o rw,subvolid=5 /dev/md127 /mnt/ro

# 2.2 删除被标快照（先删最深层）
btrfs subvolume delete /mnt/ro/default/<261所在路径>    # 路径以 subvolume list 为准
# 若 260 也是快照且不需要，一并删；只删被标的也可以（最小改动）

# 2.3 （重要）采样确认 257 的 inode 污染情况——在飞牛上跑也行：
#   inspect-syno-flags.sh /dev/md3 257
# 有 0x?0000000 大值 → 必须搬家；全小值 → 跳过 2.4~2.6，直接去第 3 步

# 2.4 新建子卷并 reflink 搬家（257 挂出来，新子卷建在顶层）
btrfs subvolume create /mnt/ro/clean
mount -t btrfs -o ro,subvolid=257 /dev/md127 /mnt/old     # 旧主卷
mkdir -p /mnt/new && mount -t btrfs -o rw,subvolid=$(btrfs subvolume list /mnt/ro | awk '/clean/{print $1}') /dev/md127 /mnt/new
# 简化：其实 clean 创建在 /mnt/ro 下后，直接用路径即可，不必再挂：
cp -a --reflink=always /mnt/old/. /mnt/ro/clean/
# 大卷可能要几十分钟（元数据操作，非数据复制）；--reflink=always 保证不会偷偷全量拷

# 2.5 对账核验（必须做）
find /mnt/old -xdev | wc -l ; find /mnt/ro/clean -xdev | wc -l     # 文件数一致
du -s --block-size=1 /mnt/old ; du -s --block-size=1 /mnt/ro/clean # 字节数一致（reflink 应完全相等）

# 2.6 核验通过 → 删旧主卷
btrfs subvolume delete /mnt/old的子卷路径      # 用 subvolume list 里的路径
```

**回滚**：2.4~2.5 期间旧卷原封未动，出任何问题 `umount` 收工即可，零损失；
2.6 是唯一不可逆步骤，必须对账通过后手动执行。

---

## 3. 其余盘的差异点（流程同上，参数不同）

| 盘 | 设备 | 主数据子卷 | 需删除的被标快照 | 特殊点 |
|---|---|---|---|---|
| md3 | /dev/md127(单盘) | 257 | 261 | 练手盘，见上 |
| md2 | /dev/md2(单盘) | 257(dir 名 default) | 638→1314→1315（先深后浅）| inode 已确认 20% 污染，必须搬家；637/1300/1301/1304/1305 也是 257 的快照，建议一并删（否则旧元数据块被快照引用，飞牛下仍是坏块）|
| sda | md(单盘)→vg1-volume_7 | **256（主子卷本身被标）** | 257/258/259/260 | 唯一主子卷被标的盘；搬家后 `btrfs subvolume set-default <clean的ID> /mnt/ro` 把新子卷设为默认 |
| TOSHIBA | md(单盘)→vg2-volume_6 | 256 | 429/430/431 | 245 个子卷的快照大户；建议把不要的快照全删（先 `subvolume list` 列全，逐个确认无独有数据），最后一步做 |

---

## 4. 清完之后：回飞牛验证（真正的考试）

1. 救援 VM 关机 → PCI 直通切回飞牛（飞牛配置不动，直接开机）；
2. SSH 执行（只读验证，不改任何东西）：
   ```bash
   mount -t btrfs -o ro,subvolid=5 /dev/md3 /mnt/check && ls /mnt/check
   ```
   **能挂上 = 清除成功**（飞牛 6.x 内核就是最严格的考官）。
3. 复跑 `inspect-syno-flags.sh /dev/md3 257`（换 clean 子卷的 ID）确认零大值；
4. 通过后再决定接管方式（挂载成存储空间），或先把数据 rsync 到别的盘双保险。

---

## 5. 出问题怎么办

| 症状 | 处置 |
|---|---|
| 救援 VM 里 mount RW 失败 | 别硬试，回飞牛贴 dmesg |
| cp 报 "failed to clone" | 个别文件（如稀疏/特殊文件）单独处理，勿改用普通复制混跑 |
| 对账数字对不上 | 停，删除前先排查（多半是挂载点嵌套），旧卷还在 |
| 删子卷报 busy | 路径下有挂载嵌套，`umount` 干净再删 |
| 飞牛回接后仍挂不上 | 复跑 inspect 脚本定位残留被标对象，逐个清 |
