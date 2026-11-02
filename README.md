# fn-syno-adopt 群晖迁移助手（飞牛 fnOS 应用）

把群晖 Basic 数据盘（分区 → mdadm → LVM `vg1/volume_1` → btrfs `@syno` 子卷）
**零拷贝接管**为飞牛原生存储空间。技术原理来自社区已验证方案
（fnOS 论坛 tid=69135 + WaterBucket0819/fnos_adopt_synology_disk），本项目将其产品化为 FPK 应用。

---

## ⚠️ 使用前必读

本工具以 **root 权限**修改存储层配置：重命名卷组（`vg1` → `trim_<uuid>`）、改写逻辑卷名、
向飞牛的 `mount` 表注册卷、`chown -R` 整卷属主、移动 `@eaDir` 目录。
写操作虽然都按「先备份 → 可回滚」设计，但**磁盘操作没有绝对安全**。

> **1. 先备份，再动手。** 重要数据必须在另一台独立设备上另有一份，并且你验证过它能恢复。
> 接管是零拷贝的，这也意味着**没有副本可以救命**——改坏的就是你唯一的那份。
>
> **2. 先拿不重要的盘练手。** 推荐在 PVE / VMware 里直通一块**空盘**，完整走一遍
> 接管 → 重启 → 回滚，确认你的机器上这套流程成立之后再碰真数据。
>
> **3. 本项目按 Apache-2.0「AS IS」提供，不附带任何明示或默示担保。**
> 包括数据丢失、系统无法启动在内的一切后果，由使用者自行承担。
>
> **4. 非官方应用。** 作者不是飞牛官方，本应用未经飞牛认证。
> Synology / 群晖 / DSM、fnOS / 飞牛 为其各自权利人的商标，本项目与它们无隶属或背书关系。

如果你不能接受以上任何一条，请不要使用本工具，改用「拷出来再建新存储空间」的稳妥路线。

## 当前状态（v0.8.18）

✅ **检测面板**（只读）：扫描块设备 / LVM / 文件系统，识别群晖特征卷。
多块群晖盘卷组全部重名 vg1，已按 **卷组 UUID** 区分定位（支持 HBA 整卡直通一次接入全部盘）。
「可接管」判定含**子卷可读 + @syno 存在**：挂载被拒（如群晖私有 root flag）或找不到 @syno
的盘不再标可接管——接管中途必失败的状态不该让用户去点。

✅ **直建 btrfs 接管（v0.6，dm-linear 方案）**：btrfs 直接建在 md 阵列上、中间**没有 LVM 层**的群晖布局
（检测类型 `direct`）支持零拷贝接管。真机验证（2026-10-30）：飞牛只认 `/dev/mapper/trim_<uuid>-0`
原生命名，直接注册 `/dev/mdN` 会被判"已移除"。方案：**dm-linear 透明包装**（LVM 底层同款机制，
不改任何数据）成原生命名 → mount 表注册（pool_type=0）→ 挂载。配套持久化：mdadm.conf（阵列自动组装）
+ systemd unit（开机自动重建 dm 映射并挂载，dm 映射本身不持久）。mount 表已有同 uuid 行时自动进入
**转正模式**（复用编号跳过 INSERT，幂等）。回滚 = 卸载 + 停用 unit + 移除 dm 映射 + 撤销注册（盘零改动）。

✅ **共享文件夹迁入个人空间（v0.8）**：把数据根顶层的共享文件夹（btrfs 子卷）移进 `/volN/<uid>/`，
即在飞牛「我的文件」中显示为文件夹。同文件系统内是 `rename`——瞬间完成、零拷贝、与数据量无关；
个人空间目录本身通常是独立子卷，此时 rename 返回 `EXDEV`，退化为 reflink 克隆 + 删源。
**代价：回群晖时路径由 `@syno/<name>` 变为 `@syno/<uid>/<name>`，DSM 里需重新指向共享文件夹（数据无损）。**

✅ **诊断与自愈（v0.7）**：挂载失败时自动抓内核日志（`dmesg`，兜底 `journalctl -k`），
把 mount 只会说的 `bad superblock` 背后的真实原因直接摆到界面上——「私有标志」与「真坏块」
的处理办法完全相反，靠猜必错。**两类群晖私有标志分开诊断**（root item 的 bit34 / inode 的 bit30），
给出各自可行的处置建议。**半接管（孤儿）盘不再隐身**：从 `/etc/lvm/archive` 反查改名前原名，
让"消失的盘"重新可见并可回滚（接管在改名之后失败、`adopted.json` 尚未写入时，盘上再无一处记得原名）。
盘符漂移改为按 dm 映射名反查物理盘。任务日志落盘 `/var/lib/fn-syno-adopt/jobs/`，
页面刷新或服务重启后仍可回看。

✅ **磁盘接管（v0.2）**：vgcfgbackup 备份 → vgrename(按 UUID)→trim_<uuid> → lvrename→0 →
临时挂载 + `btrfs set-default @syno` → 挂载 /volN → pg_dump 备份 mount 表 → INSERT 注册。
计划（Plan）/执行（Execute）分离；DRYRUN 演练；正式执行需输入 ADOPT；自动写
`/etc/modules-load.d` 持久化 crc32c；trimacl 挂载选项失败自动降级。

✅ **所有权修复（v0.3）**：流式 find 扫描属主分布（uid:gid 直方图，支持千万级文件）→
chown -R 到飞牛用户 + 可选 `setfacl -R -b` 清群晖遗留 ACL。目标 uid 必须存在于
/etc/passwd（防整卷变无效属主）；正式执行需输入 REPAIR。

✅ **@eaDir 清理（v0.4）**：find -prune 扫描 + du 分批统计 → **移入卷根回收站**
`/volN/.fn-syno-adopt-trash/<时间戳>/`（**rm 永不进白名单**），最终删除由用户在
飞牛文件管理器手动清空，可随时原样移回。正式执行需输入 CLEAN。

✅ **一键回滚（v0.5）**：接管成功会写入 `/var/lib/fn-syno-adopt/adopted.json`；
「已接管卷」列表只读这份记录——**飞牛原生存储空间同样叫 trim_xxx/0，绝不列入**，
避免误回滚用户自己的存储池。回滚 = 卸载 /volN → 卷组名改回 vg1 → 逻辑卷名改回 volume_1 →
删除 mount 表注册（先 pg_dump 备份）。**含 vg1 重名冲突检测**：整卡直通下多块群晖盘都叫 vg1，
若还有别的卷占着该名，LVM 拒绝改名，会明确提示"先回滚占用 vg1 的那块"（含占用者 UUID 与物理盘）。
正式执行需输入 ROLLBACK。

✅ **流程引导（v0.5.1）**：清理工具默认路径自动指向最近接管的卷（不再写死 /vol1 系统卷，
优先级：上次手工扫过的路径 > 最近接管挂载点 > /vol1）；接管完成后检测面板给出
"盘去哪了"的绿色说明条（改名 trim_xxx 属正常）+ 下一步按钮；侧栏迷你流程条与
"任务运行中"指示（切页也能看到进度并跳回）；接管/修复/清理各自有完成卡片与后续 checklist。

✅ **阵列开机自动组装（v0.5.3）**：接管成功后把 `ARRAY ... UUID=...` 追加进
`/etc/mdadm/mdadm.conf`（带 `# fn-syno-adopt` 标记）。群晖用的是 1.2 超级块，
内核不会自动组装——没有这行，重启后阵列可能不出现、/volN 挂载失败。
只追加、不覆盖；同一 UUID 不重复写；回滚时按 UUID 精确移除自己写过的行
（其余内容一个字节不动，有单测固化）。单盘 Basic 同样受益。

✅ **类型识别（v0.5.3，检测层）**：候选卷按证据分为
**单盘 Basic**（单 PV 且落在单块物理盘）/ **RAID**（单个 md 阵列、成员 ≥2）/
**不支持**（真跨盘卷、多个阵列、阵列降级或重建中）。
关键区分点：**多块物理盘 ≠ RAID** —— PV 在同一个 md 阵列上才是 RAID 候选，
PV 散在多块盘/多个阵列上仍是跨盘卷。RAID 当前**只识别不接管**（尚未实现，见路线图）。

✅ **目标卷下拉选择（v0.5.4）**：清理工具不再手输路径——下拉列出本机**实际挂载**的存储空间
（`/volN` + 文件系统 + 总容量 + 已用），并标注「群晖接管盘」/「飞牛系统卷」，
从根上杜绝拼错路径、选到不存在的目录、误把 /vol1 系统卷当目标。
需要子目录时可切「手动输入」（此时会给出格式警告）。数据来自
`/api/volumes`（/proc/mounts + lvs + df + 接管记录，全程只读）。

🔒 **安全不变量**：所有系统命令经 commander 白名单（argv 直传、无 shell）；
postgres 访问仅允许 `sudo -u postgres psql|pg_dump`（sudo 缺失时自动降级 Debian 自带 `runuser`）；
路径校验 `^/vol\d+(/.*)?`；写操作全局互斥（同一时刻一个任务）；异步 job + 轮询日志；
任务 goroutine 带 panic 兜底，绝不会出现"写锁永不释放"。

📌 明确不做：DSM 用户/密码迁移、家目录 bind。

📌 **能不能用在你的机器上**：先看 [已知限制与不支持场景](#已知限制与不支持场景)——
尤其是「DSM 7 私有 btrfs 标志」那一类，它不是 bug，是内核校验。

## 目录结构

```
├── manifest            # FPK 元数据（INI）
├── config/privilege    # run-as root（接管需要；检测阶段只读）
├── config/resource     # 共享目录声明
├── cmd/main            # 生命周期脚本 start/stop/status
├── app/ui/config       # 桌面图标入口
├── wizard/install      # 安装向导（端口）
├── server/             # Go 后端
│   ├── main.go         # 入口：API + 内嵌前端
│   ├── commander/      # ★ 系统命令唯一出口：白名单 + DRYRUN + argv 传参防注入
│   ├── adopt/          # 接管计划/执行 + 接管记录(adopted.json) + 回滚
│   ├── owner/          # 所有权扫描与修复
│   ├── eadir/          # @eaDir 扫描与"移入回收站"
│   ├── jobs/           # 异步任务（写操作互斥 + panic 兜底）
│   └── api/            # 只读 API：/api/system /api/disks；写操作 /api/adopt|owner|eadir|rollback
├── frontend/           # Vue3 + Vite + TS 前端
├── server/web/dist/    # 内嵌前端产物（仓库内置一个无构建依赖的占位页，Vue 构建后覆盖）
├── tools/gen_icons.py  # 图标生成（纯 stdlib）
└── build.sh            # 一键构建：前端 → Go 交叉编译 → fnpack build
```

## 路线图

- **RAID（成员 ≥2）接管 —— 尚未实现，当前只识别不接管。**
  要做的事：数据 LV 不再假设 `volume_1`（取 VG 内最大的 LV，自然排除群晖 12M 保留卷）、
  接管前置强制 `State: clean`、接管页按类型分标签、回滚时还原原始 LV 名。
  直建 btrfs（`direct`）接管已在 v0.6 落地，RAID 与之的区别只在"数据 LV 多了一层 md 聚合"。
  **欢迎有 RAID 测试环境的同学接手**——这一项缺的不是代码，是能反复砸的测试盘。

## 已知限制与不支持场景

**先看这一节，再决定要不要用。** 下面每一条都是「工具会拒绝、或必然失败」，不是 bug。

### ① 群晖 DSM 7 的私有 btrfs 标志 —— 最常见的一类

**先摸底再动手**：`docs/inspect-syno-flags.sh` 只读统计私有标记在盘上的分布，
能分清下面两层各占多少。先确认中的是哪一层，再选后面的路——两层能做的事不一样。

**现象**：检测面板显示「不可接管」，或手动 mount 报 `can't read superblock` / `open_ctree failed: -5`。
`dmesg` 里能看到两种报错之一：

```
invalid root flags, have 0x400000000 expect mask 0x1000000000001    # root item 层（bit34）
corrupt leaf: ... ino=xxxx, unknown incompat flags detected: 0x40000000   # inode 层（bit30）
```

**原因（重要）**：**盘没坏，数据也没坏，树根也没有损坏。** 是 DSM 7 魔改过的 btrfs 往
共享文件夹子卷（root item）和其中部分文件的 inode 上写入了私有标志位，主线内核
（5.4+ 起收紧的 `tree-checker`）严格校验后拒绝打开。用户态的 `btrfs-progs` 不校验，
所以 `btrfs restore -l` 反而能完整列出所有子卷。

**三条路，按「对环境的额外要求」从低到高排**（没有一条是零风险，先看懂再动手）：

**1. 拷出来 —— 最稳，飞牛本机就能做**

`btrfs-progs` 是用户态、不做 tree-checker 校验，所以**在这台飞牛上就能读**。
用 `docs/rescue-data.sh` 跑 `btrfs restore`，先 `-D` 空跑评估再真跑，全程不写源盘。
代价是要另有一块够大的目标盘，等于搬一次数据。**拿不准用哪条就选这条。**

**2. 原地重建子卷 —— 省一块盘，但需要旧内核环境**

`docs/rebuild-subvol.sh`：在**能正常读盘的内核**里，把内容 reflink 克隆进一个全新子卷——
新 inode、新 root item 都由那个内核生成，天然不带私有标志；reflink 正常生效时
不搬字节、不占额外空间。流程 `plan → run → verify → cleanup`，`run` 全程不删不改旧子卷，
失败就退出、旧数据原样在那儿。本项目的四块盘抢救走的就是这条路。

有两个坑必须知道：
- **cleanup 之后旧树仍留在盘上**（有意留作回退保险），要删掉它才算彻底干净——
  root item 层的标志是随旧树一起消失的
- 删旧子卷**之前**必须先 `set-default` 到新树，否则删默认子卷会报 `EPERM`

**3. 给飞牛内核打补丁 —— 一劳永逸，但要会编内核**

放宽 `tree-checker.c` 里对 root flags 与 inode flags 的两处校验（社区有 6.x 方案，
如 `limitcool/synology-btrfs-kernel-patch`）。代价是每次升级飞牛内核都要重打一遍。

**关于「哪个内核可以」—— 别只看版本号，要看具体构建：**

| 内核 | 能否读带标志的盘 | 说明 |
|---|---|---|
| 4.4 | ✅ 稳妥 | DSM 7 自身内核，最保险的锚点 |
| 4.15.0-20 | ✅ 可以 | Ubuntu 18.04.1 原始内核，本项目抢救时用的就是它 |
| 4.15.0-213 | ❌ 不行 | ESM 后期构建，加固已被 backport 进去 |
| 5.4 及以上主线 | ❌ 不行 | tree-checker 默认严格 |

同为 4.15，早期构建没这批 backport、后期有。所以「内核小于 5.4 就行」是错的，
「装个 4.15 就行」也是错的——版本号相同，行为可能完全相反。

**无效的做法**：`btrfs check --repair` 对这类标志无效；直接 hex 改标志位也不可取
（inode 要逐个清，社区实测失败）。

### ② 其他不支持的情况

| 场景 | 处理 |
|---|---|
| 真跨盘卷（PV 散在多块盘 / 多个 md 阵列，不是单一阵列） | 检测阶段拒绝接管 |
| RAID 阵列（同一 md 阵列、成员 ≥2） | **当前只识别、不接管**，界面会标出阵列级别与健康度 |
| 阵列处于降级 / 重建 / reshape 中 | 一律拒绝，先修好阵列再谈接管 |
| 非 btrfs 的群晖卷（如 ext4） | 子卷模型不成立，不支持 |
| ARM 机型（部分飞牛盒子） | 只构建了 `linux/amd64`，没有 arm64 产物 |
| DSM 用户/密码迁移、家目录 bind | **明确不做**（见上文） |

> 检测面板的「可接管」判定包含**子卷可读 + `@syno` 存在**两个必要条件。
> 挂载被拒（如上面 ①）或找不到 `@syno` 的盘不会被标成可接管——
> 接管中途必然失败的状态，不该让用户去点。

## 待真机验证（尚未在真机确认的路径）

以下几项本地只做到编译 / 类型检查 / 单测 / DRYRUN 演练级别，
**在你的机器上可能成立也可能不成立**，遇到问题请附 `dmesg | tail -50` 与任务日志反馈。

1. fnOS 上 `sudo`（或兜底 `runuser`）+ postgres peer 认证 + `trim` 库名是否成立
2. 接管完成后飞牛存储管理器是否立即显示新存储空间（日志已提示"刷新页面或重启系统"）
3. 多块群晖盘（多 vg1 重名）场景下 `vgcfgbackup <VG-UUID>` 是否被 LVM 接受
   （已按 UUID 定位并把失败降级为警告，不会中断流程）
4. `mount` 表插入是否只需 id/uuid 两列（其余走默认值）
5. ~~直建 btrfs 接管：mount 表 uuid 格式~~ **（已解决，2026-10-30）**——
   直接注册 `/dev/mdN` 被判"已移除"；改为 **dm-linear 包装成 `/dev/mapper/trim_<uuid>-0`**
   后真机验证存储管理器正常认账。
   **待验证**：重启后 mdadm.conf + systemd unit 是否让存储空间自动恢复
   （探查脚本：`docs/probe-fnos-mount.sh` / `probe-fnos-mount2.sh`；试验脚本 `try-dmlinear.sh`）

## 环境要求

- Go >= 1.22（用了 1.22 的路由语法）、Node.js >= 18、fnpack CLI
- 测试目标机：飞牛 fnOS x86_64（你自己的 PVE 虚拟机 + 快照 = 理想测试场）

## 快速开始

```bash
# 本地开发（前端热更新 + Go 后端）
cd frontend && npm install && npm run dev        # 终端 1
cd server && go run . -port 18099                # 终端 2（Windows 上也能跑）
# 演练模式：SYNOKIT_DRYRUN=1 go run .            # 所有写操作只打印不执行
# 类型检查：cd frontend && npm run type-check    # vue-tsc（项目内固定版本，勿用 npx 临时拉）

# 一键构建 FPK（Linux / WSL / Git Bash）
./build.sh
# 安装到飞牛：应用中心 → 手动安装 上传 .fpk，或
# appcenter-cli install-local fn-syno-adopt_x.y.z.fpk
```

## 安全设计（不可妥协项）

1. **命令白名单**：所有系统命令只能经 `commander.Run()` 执行，新增命令必须登记
2. **argv 传参**：永不拼接 shell 字符串，用户输入视为不可信
3. **DRYRUN**：`SYNOKIT_DRYRUN=1` 时所有写操作只打印计划
4. **先只读后写入**：检测阶段不修改数据（`/api/lvm/activate`、`/api/md/assemble` 会激活卷与阵列，
   但不改写磁盘数据）；接管阶段每步先备份（pg_dump / vgcfgbackup）
5. **rm 永不进白名单**：删除类功能单独设计
6. **可回滚**：接管写记录、回滚只读记录，避免误伤飞牛原生存储空间

## 运行模式（v0.1.0 起：统一网关）

- **统一网关模式（默认，飞牛内安装时）**：后端监听 `${TRIM_APPDEST}/app.sock`（权限 0770），
  飞牛网关校验登录态后转发 `/app/fn-syno-adopt/...`。**不占 TCP 端口、不暴露局域网、复用飞牛登录鉴权、
  HTTPS 域名访问无混合内容问题**。后端自动剥离网关前缀；前端全部使用相对路径（vite `base: './'`）。
- **开发模式**：无 `TRIM_APPDEST` 环境时回落 TCP `:18099`，本地 `go run .` + `npm run dev` 照常调试。
- `app/ui/config` 关键字段：`type: iframe`（桌面内嵌窗口）+ `gatewaySocket: app.sock` + `gatewayPrefix: /app/fn-syno-adopt`；
  `manifest` 不再声明 service_port（网关模式下无 TCP 端口）。

## FPK schema 校准记录（已通过 fnpack 1.2.3 实测）

manifest 必须包含的字段（从 `fnpack create` 官方模板确认）：
`appname / version / display_name / desc（不是 description）/ platform / source（thirdparty）/
maintainer / distributor / desktop_uidir（=ui）/ desktop_applaunchname（=appname.Application）`

- 桌面图标放 `app/ui/images/icon_64.png` 与 `icon_256.png`，ui/config 里写 `images/icon_{0}.png`
- `config/resource` schema 为 `{"data-share": {"shares": []}}`
- `wizard/` 为可选组件（模板为空目录）；安装向导 schema 待查，v0.1 暂不使用（端口固定 18099）
- fnpack 二进制：https://developer.fnnas.com/docs/cli/fnpack/ （本仓库 tools/ 下有一份，已 gitignore）

## 分发计划

- 源码：GitHub（Release 放 `.fpk`）
- 第三方源：fnrc.json 放 **Gitee**（国内直连；不要用 GitHub raw），申请社区聚合源收录
- 加分项：报名飞牛先锋开发者计划，就该应用涉及 root 权限与存储层接管与官方沟通

**暂不上飞牛官方应用商店。** 本工具以 root 权限改写存储层配置，一旦上架就意味着
以「官方分发方」身份为别人的数据背书，而它的正确性高度依赖具体机型、固件与磁盘布局。
GitHub Release + 第三方源的形式，能让使用者清楚知道自己装的是第三方工具、
并自愿承担验证责任——这个边界对双方都更健康。

## 许可证

[Apache License 2.0](LICENSE)，版权与第三方归属声明见 [NOTICE](NOTICE)。

选 Apache-2.0 而不是 MIT 的原因：它在免责之外还有**明确的商标授权边界**（第 6 条）——
任何人不得用「群晖」「飞牛」「Synology」暗示本工具获得官方认可，也不得用作者名义做推广。
本项目与上述公司无任何隶属或背书关系。

