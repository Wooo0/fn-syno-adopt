// 后端 API 类型定义与请求封装
export interface SystemInfo {
  hostname: string
  os: string
  kernel: string
  dryRun: boolean
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
  fs: string
  subvols: string[] | null   // 后端保证非 null，前端仍做兼容
  adoptable: boolean
  notes: string[] | null

  // 类型与阵列信息（分类由检测决定）
  kind: 'basic' | 'raid' | 'unsupported'
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
  vgUuid: string
  oldVG: string
  newVG: string
  dmPath: string
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
}

export interface EaDirScanResult {
  path: string
  count: number
  bytes: number
  bytesHuman: string
  sample: string[] | null
  truncated: boolean
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

  adoptPlan: (vgUuids: string[]) =>
    post<{ ok: boolean; plans: AdoptPlan[] | null; errors: { vgUuid: string; error: string }[] | null }>(
      'api/adopt/plan', { vgUuids }),

  adoptExec: (vgUuid: string, dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/adopt/exec', { vgUuid, dryRun, confirm }),

  adopted: () => get<{ ok: boolean; volumes: AdoptedVolume[] | null }>('api/adopted'),

  adoptRollback: (vgUuid: string, dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/adopt/rollback', { vgUuid, dryRun, confirm }),

  ownerScan: (path: string) =>
    post<{ ok: boolean; data: OwnerScanResult }>('api/owner/scan', { path }),

  ownerRepair: (req: { path: string; uid: number; gid: number; removeAcl: boolean; dryRun: boolean; confirm: string }) =>
    post<{ ok: boolean; jobId: string }>('api/owner/repair', req),

  eadirScan: (path: string) =>
    post<{ ok: boolean; data: EaDirScanResult }>('api/eadir/scan', { path }),

  eadirClean: (path: string, dryRun: boolean, confirm: string) =>
    post<{ ok: boolean; jobId: string }>('api/eadir/clean', { path, dryRun, confirm }),

  job: (id: string) => get<{ ok: boolean; job: Job }>(`api/jobs/${encodeURIComponent(id)}`),
  jobs: () => get<{ ok: boolean; jobs: Job[] | null }>('api/jobs'),
}
