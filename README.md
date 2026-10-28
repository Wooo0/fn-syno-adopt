# fn-syno-adopt 群晖迁移助手（飞牛 fnOS 应用）

把群晖 Basic 数据盘（分区 → mdadm → LVM `vg1/volume_1` → btrfs `@syno` 子卷）
**零拷贝接管**为飞牛原生存储空间。技术原理来自社区已验证方案
（fnOS 论坛 tid=69135 + WaterBucket0819/fnos_adopt_synology_disk），本项目将其产品化为 FPK 应用。

## 当前状态（v0.5.4 全功能 + 可回滚）

✅ **检测面板**（只读）：扫描块设备 / LVM / 文件系统，识别群晖特征卷。
多块群晖盘卷组全部重名 vg1，已按 **卷组 UUID** 区分定位（支持 HBA 整卡直通一次接入全部盘）。

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
PV 散在多块盘/多个阵列上仍是跨盘卷。RAID 当前**只识别不接管**（v0.6 支持）。

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

- **v0.6（规划）**：RAID 接管 —— 数据 LV 不再假设 `volume_1`（取 VG 内最大的 LV，
  自然排除群晖 12M 保留卷）、接管前置强制 `State: clean`、接管页按类型分标签、
  回滚还原原始 LV 名。等单盘 Basic 真机验证通过后再动工。

## 待真机验证（本地只做到编译/类型/单测/演练级别）

1. fnOS 上 `sudo`（或兜底 `runuser`）+ postgres peer 认证 + `trim` 库名是否成立
2. 接管完成后飞牛存储管理器是否立即显示新存储空间（日志已提示"刷新页面或重启系统"）
3. 多块群晖盘（多 vg1 重名）场景下 `vgcfgbackup <VG-UUID>` 是否被 LVM 接受
   （已按 UUID 定位并把失败降级为警告，不会中断流程）
4. `mount` 表插入是否只需 id/uuid 两列（其余走默认值）

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

- 源码：GitHub（Release 放 .fpk）
- 第三方源：fnrc.json 放 **Gitee**（国内直连；不要用 GitHub raw），申请社区聚合源收录
- 加分项：报名飞牛先锋开发者计划，就该应用涉及 root 权限与存储层接管与官方沟通
