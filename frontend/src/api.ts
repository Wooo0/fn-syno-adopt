// 后端 API 类型定义与请求封装
export interface SystemInfo {
  hostname: string
  os: string
  kernel: string
  dryRun: boolean
  version?: string   // 后端由 manifest 注入；旧后端无此字段，侧栏回退不显示
}

export interface SynoCandidate {
  disk: string
  model: string
  serial: string
  vg: string
  vgUuid: string
  lv: string
  dmPath: string
  lvSize: string
  lvActive: boolean
  devReady: boolean         // 设备节点是否可用（卷组已激活）——读不到就不能算可接管
  fs: string
  subvols: string[] | null   // 后端保证非 null，前端仍做兼容
  adoptable: boolean
  adopted?: boolean          // 已由本工具接管（终态）：不可再勾选，徽章显示"已接管"
  notes: string[] | null

  // 类型与阵列信息（分类由检测决定）
  // basic=单盘(有LVM) raid=阵列(有LVM) direct=btrfs直建在阵列上(无LVM) unsupported=其它
  kind: 'basic' | 'raid' | 'direct' | 'unsupported'
  mdDev: string
  raidLevel: string
  raidMembers: number
  raidActive: number
  raidHealth: string
  raidSize: string
  physDisks: string[] | null
}

export interface Disk {
  name: string
  size: string
  type: string
  fstype?: string
  model?: string
  serial?: string
  children?: Disk[]
}

export interface AdoptPlan {
  kind?: 'lvm' | 'direct'    // v0.5.x 后端无此字段，按 lvm 处理
  vgUuid: string              // LVM：卷组 UUID；direct：md 阵列 UUID
  oldVG: string
  oldLV: string
  newVG: string
  dmPath: string
  mdDev?: string              // direct：底层 md 阵列设备（/dev/mdN）
  volId: number
  mountPoint: string
  muuid: string
  synoSubvolId: number
  disk: string
  lvSize: string
  steps: string[]
  warnings: string[] | null
}

export interface AdoptedVolume {
  vgUuid: string
  vg: string
  lv: string
  oldVG: string
  oldLV?: string          // 原逻辑卷名（半接管盘靠 LVM 归档回溯）
  orphan?: boolean        // 接管中途失败：卷已改名但没记录/没挂载/没注册
  dmPath: string
  lvSize: string
  disk: string
  volId: number
  mountPoint: string
  mounted: boolean
  registered: boolean
  lvActive: boolean
  time: string
  rollbackable: boolean
  vgNameBusy: boolean
  busyBy: string
  notes: string[]
}

export interface StepLog {
  name: string
  command: string
  status: 'ok' | 'fail' | 'skip' | 'pending'
  detail: string
}

export interface Job {
  id: string
  kind: string
  status: 'running' | 'done' | 'error'
  error?: string
  steps: StepLog[] | null
  start?: string
  end?: string
}

export interface OwnerStat {
  uid: number
  gid: number
  files: number
  dirs: number
  kind: string
}

export interface SuggestUser {
  uid: number
  gid: number
  name: string
}

export interface OwnerScanResult {
  path: string
  total: number
  stats: OwnerStat[] | null
  truncated: boolean
  suggest: SuggestUser | null
  noPermFiles?: number   // POSIX 权限位全 000 的文件（群晖 synoacl 遗留）
  noPermDirs?: number    // 同上的目录——飞牛文件服务进不去的元凶
}

export interface EaDirScanResult {
  path: string
  count: number
  bytes: number
  bytesHuman: string
  sample: string[] | null
  truncated: boolean
}

export interface ShareItem {
  name: string
  isSubvol: boolean
  system: boolean
}

export interface ShareView {
  mountPoint: string
  uid: string            // 个人空间目录名（第一个数字目录）
  uidDirs: string[] | null
  uidExists: boolean
  items: ShareItem[] | null      // 可迁移候选
  system: ShareItem[] | null     // 系统目录（不给迁，只展示）
  migrated: string[] | null      // 已迁入个人空间的目录
}

export interface ShareMoveResult {
  name: string
  ok: boolean
  // 成功时的补充说明：跨子卷迁移是"reflink 克隆 + 删源"，不是瞬间 rename，
  // 必须让用户知道本次实际做了什么。
  note?: string
  err?: string
}

export interface Volume {
  path: string
  device: string
  vg: string
  lv: string
  fs: string
  total: number
  used: number
  avail: number
  totalHuman: string
  usedHuman: string
  adopted: boolean
  disk: string
  adoptTime: string
  isSystem: boolean
}

export interface SysUsers {
  suggest: SuggestUser | null
  crcLoaded: boolean
}

// 统一网关模式下页面挂在 /app/fn-syno-adopt/ 前缀下，
// API 调用必须用相对路径（不带前导斜杠），由后端剥离前缀后路由。
async function get<T>(url: string): Promise<T> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`${url} 请求失败: ${res.status}`)
  return res.json()
}

async function post<T>(url: string, body: unknown): Promise<T> {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const json = await res.json().catch(() => ({}))
  if (!res.ok || json.ok === false) {
    throw new Error(json.error || `${url} 请求失败: ${res.status}`)
  }
  return json
}

export const api = {
  system: () => get<{ ok: boolean; data: SystemInfo }>('api/system'),
  disks: () =>
    get<{
      ok: boolean
      candidates: SynoCandidate[] | null
      disks: Disk[] | null
    }>('api/disks'),
  sysUsers: () => get<SysUsers>('api/sysusers'),

  volumes: () =>
    get<{ ok: boolean; volumes: Volume[] | null; recommend: string }>('api/volumes'),

  mdScan: () =>
    get<{ ok: boolean; arrays: string[] | null }>('api/md/scan'),
  mdAssemble: () =>
    post<{ ok: boolean; output: string }>('api/md/assemble', {}),

  lvmActivate: (vgUuid: string) =>
    post<{ ok: boolean; output: string }>('api/lvm/activate', { vgUuid }),

  adoptPlan: (ids: string[]) =>
    post<{ ok: boolean; plans: AdoptPlan[] | null; errors: { vgUuid: string; error: string }[] | null }>(
      'api/adopt/plan', { vgUuids: ids }),

  // id：LVM 传 vgUuid；直建（direct）传 mdDev（/dev/mdN）
  adoptExec: (id: string, dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/adopt/exec', { vgUuid: id, dryRun, confirm }),

  adopted: () => get<{ ok: boolean; volumes: AdoptedVolume[] | null }>('api/adopted'),

  adoptRollback: (vgUuid: string, dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/adopt/rollback', { vgUuid, dryRun, confirm }),

  // 逐块回滚所有可回滚的卷（串行，任一块失败即中止）
  adoptRollbackAll: (dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/adopt/rollback-all', { dryRun, confirm }),

  ownerScan: (path: string) =>
    post<{ ok: boolean; data: OwnerScanResult }>('api/owner/scan', { path }),

  ownerRepair: (req: { path: string; uid: number; gid: number; removeAcl: boolean; fixPerm: boolean; dryRun: boolean; confirm: string }) =>
    post<{ ok: boolean; jobId: string }>('api/owner/repair', req),

  eadirScan: (path: string) =>
    post<{ ok: boolean; data: EaDirScanResult }>('api/eadir/scan', { path }),

  eadirClean: (path: string, dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/eadir/clean', { path, dryRun, confirm }),

  sharesScan: (mp: string) =>
    get<{ ok: boolean; data: ShareView }>(`api/shares?mp=${encodeURIComponent(mp)}`),

  sharesMove: (mountPoint: string, uid: string, names: string[]) =>
    post<{ ok: boolean; results: ShareMoveResult[] }>('api/shares/move', { mountPoint, uid, names }),

  job: (id: string) => get<{ ok: boolean; job: Job }>(`api/jobs/${encodeURIComponent(id)}`),
  jobs: () => get<{ ok: boolean; jobs: Job[] | null }>('api/jobs'),
  jobDelete: (id: string) =>
    post<{ ok: boolean }>(`api/jobs-delete/${encodeURIComponent(id)}`, {}),
}
