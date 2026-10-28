<script setup lang="ts">
import { computed, onActivated, onMounted, ref } from 'vue'
import {
  api, type AdoptedVolume, type EaDirScanResult, type Job,
  type OwnerScanResult, type SuggestUser,
} from '../api'
import UiButton from './ui/UiButton.vue'
import UiCard from './ui/UiCard.vue'
import UiBadge from './ui/UiBadge.vue'
import UiEmpty from './ui/UiEmpty.vue'
import JobLog from './JobLog.vue'
import DoneCard from './DoneCard.vue'

type Tab = 'owner' | 'eadir'
const tab = ref<Tab>('owner')

// —— 默认路径推断 ——
// /vol1 是飞牛系统卷，写死成默认值会让用户白扫几分钟，甚至对自己的系统卷动 chown。
// 优先级（显式行为 > 自动推断 > 兜底）：
//   1. localStorage：用户上一次扫描成功的路径（显式选择，最贴近意图）
//   2. 最近一条接管记录的 mountPoint（群晖盘一般落在 /vol2+）
//   3. /vol1
const LS_KEY_OWNER = 'fn-syno-adopt.lastPath.owner'
const LS_KEY_EA = 'fn-syno-adopt.lastPath.eadir'

const adopted = ref<AdoptedVolume[]>([])
const latestAdopted = computed<AdoptedVolume | null>(() => {
  if (!adopted.value.length) return null
  // VolID 最大的即最近接管的卷
  return adopted.value.reduce((a, b) => (b.volId > a.volId ? b : a))
})

function defaultPath(lsKey: string): string {
  const saved = localStorage.getItem(lsKey)
  if (saved) return saved
  if (latestAdopted.value?.mountPoint) return latestAdopted.value.mountPoint
  return '/vol1'
}

// —— 所有权修复 ——
const ownerPath = ref(defaultPath(LS_KEY_OWNER))
const ownerScanRes = ref<OwnerScanResult | null>(null)
const ownerScanning = ref(false)
const ownerRunning = ref(false)
const ownerConfirm = ref('')
const ownerDone = ref(false)
const removeAcl = ref(true)
const uid = ref<number | null>(null)
const gid = ref<number | null>(null)
const uidName = ref('')
const ownerJobId = ref<string | null>(null)
const ownerError = ref('')

const canOwnerRepair = ref(false) // 输入 REPAIR 后解锁

async function scanOwner() {
  ownerScanning.value = true
  ownerError.value = ''
  ownerScanRes.value = null
  ownerDone.value = false
  try {
    const res = await api.ownerScan(ownerPath.value)
    ownerScanRes.value = res.data
    localStorage.setItem(LS_KEY_OWNER, ownerPath.value) // 记住这次显式选择的路径
  } catch (e) {
    ownerError.value = e instanceof Error ? e.message : String(e)
  } finally {
    ownerScanning.value = false
  }
}

// 刷接管记录（用于默认路径推断与「最近接管」按钮）；失败不影响手工填路径
async function refreshAdopted() {
  try {
    const res = await api.adopted()
    adopted.value = res.volumes ?? []
  } catch { /* 忽略 */ }
}

// 把最近接管的卷填进路径框（覆盖当前值，作为显式选择被记住）
function fillLatest() {
  const p = latestAdopted.value?.mountPoint
  if (!p) return
  ownerPath.value = p
  eaPath.value = p
  localStorage.setItem(LS_KEY_OWNER, p)
  localStorage.setItem(LS_KEY_EA, p)
}

function applySuggest(s: SuggestUser | null) {
  if (!s) return
  uid.value = s.uid
  gid.value = s.gid
  uidName.value = s.name
}

async function runOwnerRepair(dryRun: boolean) {
  if (uid.value === null || gid.value === null) {
    ownerError.value = '请先填写目标用户 uid/gid'
    return
  }
  ownerRunning.value = true
  ownerError.value = ''
  try {
    const res = await api.ownerRepair({
      path: ownerPath.value,
      uid: uid.value,
      gid: gid.value,
      removeAcl: removeAcl.value,
      dryRun,
      confirm: dryRun ? '' : 'REPAIR',
    })
    ownerJobId.value = res.jobId
  } catch (e) {
    ownerError.value = e instanceof Error ? e.message : String(e)
    ownerRunning.value = false
  }
}

function onOwnerJobDone(job: Job | null) {
  ownerRunning.value = false
  ownerDone.value = !!job && job.status === 'done'
}

// —— @eaDir 清理 ——
const eaPath = ref(defaultPath(LS_KEY_EA))
const eaRes = ref<EaDirScanResult | null>(null)
const eaScanning = ref(false)
const eaRunning = ref(false)
const eaConfirm = ref('')
const eaJobId = ref<string | null>(null)
const eaError = ref('')
const eaDone = ref(false)
const eaTrash = ref('') // 清理完成后的回收站路径

const canEaClean = ref(false)

async function scanEa() {
  eaScanning.value = true
  eaError.value = ''
  eaRes.value = null
  eaDone.value = false
  try {
    const res = await api.eadirScan(eaPath.value)
    eaRes.value = res.data
    localStorage.setItem(LS_KEY_EA, eaPath.value)
  } catch (e) {
    eaError.value = e instanceof Error ? e.message : String(e)
  } finally {
    eaScanning.value = false
  }
}

async function runEaClean(dryRun: boolean) {
  eaRunning.value = true
  eaError.value = ''
  try {
    const res = await api.eadirClean(eaPath.value, dryRun, dryRun ? '' : 'CLEAN')
    eaJobId.value = res.jobId
  } catch (e) {
    eaError.value = e instanceof Error ? e.message : String(e)
    eaRunning.value = false
  }
}

function onEaJobDone(job: Job | null) {
  eaRunning.value = false
  eaDone.value = !!job && job.status === 'done'
  if (eaDone.value) {
    const root = eaPath.value.match(/^\/vol\d+/)?.[0] ?? eaPath.value
    eaTrash.value = `${root}/.fn-syno-adopt-trash/`
  }
}

onMounted(async () => {
  try {
    const s = await api.sysUsers()
    applySuggest(s.suggest)
  } catch { /* 忽略：用户可手填 */ }
  await refreshAdopted()
  // 用户从未手工扫过时（无 localStorage），跟随最近接管的卷
  if (!localStorage.getItem(LS_KEY_OWNER)) ownerPath.value = defaultPath(LS_KEY_OWNER)
  if (!localStorage.getItem(LS_KEY_EA)) eaPath.value = defaultPath(LS_KEY_EA)
})

// KeepAlive 缓存后 onMounted 只跑一次，切回本页刷新接管记录，
// 保证「最近接管：/volN」按钮指向最新的卷。
// 不自动改写输入框：用户可能已手工填了路径，改了才是显式选择，由按钮触发。
// 同上：跳过 KeepAlive 首次挂载那次触发（onMounted 里已经拉过）
let activatedOnce = false
onActivated(() => {
  if (!activatedOnce) {
    activatedOnce = true
    return
  }
  refreshAdopted()
})
</script>

<template>
  <div class="tabs">
    <button :class="{ active: tab === 'owner' }" @click="tab = 'owner'">所有权修复</button>
    <button :class="{ active: tab === 'eadir' }" @click="tab = 'eadir'">@eaDir 清理</button>
  </div>

  <!-- ============ 所有权修复 ============ -->
  <template v-if="tab === 'owner'">
    <UiCard title="扫描所有权分布">
      <template #desc>
        接管后的卷上文件属主仍是群晖 uid（如 admin=1026），飞牛用户无法读写。先扫描确认属主分布，再决定修复目标。
      </template>
      <template #actions>
        <UiBadge v-if="ownerScanRes" type="info">{{ ownerScanRes.total }} 项</UiBadge>
        <UiButton :disabled="ownerScanning || ownerRunning" @click="scanOwner">
          {{ ownerScanning ? '扫描中…' : '扫描' }}
        </UiButton>
      </template>

      <div class="path-row">
        <input v-model="ownerPath" class="path-input mono" :disabled="ownerRunning" spellcheck="false" />
        <UiButton v-if="latestAdopted?.mountPoint" variant="ghost" @click="fillLatest">
          最近接管：{{ latestAdopted.mountPoint }}
        </UiButton>
        <p class="hint">
          仅允许 /volN 开头的路径（默认已指向最近接管的卷，/vol1 是飞牛系统卷，一般不是你要修的盘）。
          大卷扫描可能耗时数分钟：百万级文件约需数分钟，可离开本页，任务在后台继续。
        </p>
      </div>

      <p v-if="ownerError" class="err-text">✗ {{ ownerError }}</p>

      <table v-if="ownerScanRes?.stats?.length">
        <thead>
          <tr><th>uid:gid</th><th>目录</th><th>文件</th><th>归类</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="s in ownerScanRes.stats" :key="`${s.uid}:${s.gid}`">
            <td class="mono">{{ s.uid }}:{{ s.gid }}</td>
            <td>{{ s.dirs }}</td>
            <td>{{ s.files }}</td>
            <td>
              <UiBadge :type="s.kind === 'syno' ? 'warn' : s.kind === 'local' ? 'ok' : 'info'">
                {{ { syno: '群晖段（需修复）', local: '本地用户段', root: 'root', system: '系统' }[s.kind] || s.kind }}
              </UiBadge>
            </td>
            <td>
              <UiButton v-if="s.kind === 'local'" variant="ghost" @click="applySuggest({ uid: s.uid, gid: s.gid, name: '' })">
                以此为目标
              </UiButton>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="ownerScanRes?.truncated" class="hint">仅显示前 50 组，其余已合并省略。</p>
    </UiCard>

    <UiCard title="执行修复">
      <template #desc>
        递归 chown 到目标用户 + 可选清除群晖扩展 ACL。执行前请先演练。
      </template>
      <div class="form-grid">
        <label>目标用户
          <span v-if="uidName" class="hint-inline mono">{{ uidName }}</span>
        </label>
        <div class="pair">
          <input v-model.number="uid" type="number" class="num-input mono" :disabled="ownerRunning" placeholder="uid" />
          <span class="colon">:</span>
          <input v-model.number="gid" type="number" class="num-input mono" :disabled="ownerRunning" placeholder="gid" />
        </div>
        <label>路径</label>
        <input v-model="ownerPath" class="path-input mono" :disabled="ownerRunning" spellcheck="false" />
        <label>扩展 ACL</label>
        <label class="check">
          <input v-model="removeAcl" type="checkbox" :disabled="ownerRunning" />
          同时执行 setfacl -R -b 清除群晖遗留 ACL（推荐）
        </label>
      </div>

      <div class="exec-btns">
        <UiButton variant="default" :disabled="ownerRunning" @click="runOwnerRepair(true)">演练</UiButton>
        <UiButton
          variant="danger" :disabled="ownerRunning || !canOwnerRepair || uid === null"
          @click="runOwnerRepair(false)"
        >正式修复</UiButton>
        <input
          v-model="ownerConfirm" class="confirm-input mono" :disabled="ownerRunning"
          placeholder="输入 REPAIR 解锁" spellcheck="false"
          @input="canOwnerRepair = ownerConfirm.trim().toUpperCase() === 'REPAIR'"
        />
      </div>
      <p class="hint">正式修复会递归改写整卷属主，耗时与文件数量成正比（百万级文件可能需要数十分钟）。任务在后台执行，可离开页面。</p>
      <JobLog :job-id="ownerJobId" @finished="onOwnerJobDone" />
      <DoneCard
        v-if="ownerDone"
        title="所有权修复完成"
        :items="[
          `已把 ${ownerPath} 下的文件属主改为 ${uid}:${gid}`,
          '到飞牛文件管理器确认你的用户可以正常读写',
          '接着做 @eaDir 清理（切换到上方标签页），清掉群晖遗留的缩略图缓存',
        ]"
      />
    </UiCard>
  </template>

  <!-- ============ @eaDir 清理 ============ -->
  <template v-else>
    <UiCard title="扫描 @eaDir">
      <template #desc>
        @eaDir 是群晖自动生成的缩略图/索引缓存目录，迁移后完全无用，且会干扰飞牛的文件索引。
      </template>
      <template #actions>
        <UiBadge v-if="eaRes" type="info">{{ eaRes.count }} 个 · {{ eaRes.bytesHuman }}</UiBadge>
        <UiButton :disabled="eaScanning || eaRunning" @click="scanEa">
          {{ eaScanning ? '扫描中…' : '扫描' }}
        </UiButton>
      </template>

      <div class="path-row">
        <input v-model="eaPath" class="path-input mono" :disabled="eaRunning" spellcheck="false" />
        <UiButton v-if="latestAdopted?.mountPoint" variant="ghost" @click="fillLatest">
          最近接管：{{ latestAdopted.mountPoint }}
        </UiButton>
        <p class="hint">仅允许 /volN 开头的路径。目录很多时扫描需数分钟，可离开本页，任务在后台继续。</p>
      </div>

      <p v-if="eaError" class="err-text">✗ {{ eaError }}</p>

      <UiEmpty v-if="eaRes && eaRes.count === 0">
        未发现任何 @eaDir
        <template #hint>该卷很干净，无需处理</template>
      </UiEmpty>

      <div v-else-if="eaRes" class="sample">
        <p class="hint">样例（{{ Math.min(eaRes.sample?.length ?? 0, 200) }}/{{ eaRes.count }}）：</p>
        <div class="sample-list mono">
          <div v-for="s in eaRes.sample ?? []" :key="s">{{ s }}</div>
        </div>
      </div>
    </UiCard>

    <UiCard title="执行清理">
      <template #desc>
        安全设计：应用绝不直接删除。@eaDir 会被移入卷根回收站
        <span class="mono">.fn-syno-adopt-trash/&lt;时间戳&gt;/</span>，
        确认无误后在飞牛文件管理器中手动清空即可完成删除；期间随时可原样移回。
      </template>
      <div class="path-row">
        <label class="form-label">路径</label>
        <input v-model="eaPath" class="path-input mono" :disabled="eaRunning" spellcheck="false" />
      </div>
      <div class="exec-btns">
        <UiButton variant="default" :disabled="eaRunning" @click="runEaClean(true)">演练</UiButton>
        <UiButton variant="danger" :disabled="eaRunning || !canEaClean" @click="runEaClean(false)">
          移入回收站
        </UiButton>
        <input
          v-model="eaConfirm" class="confirm-input mono" :disabled="eaRunning"
          placeholder="输入 CLEAN 解锁" spellcheck="false"
          @input="canEaClean = eaConfirm.trim().toUpperCase() === 'CLEAN'"
        />
      </div>
      <JobLog :job-id="eaJobId" @finished="onEaJobDone" />
      <DoneCard
        v-if="eaDone"
        title="@eaDir 已移入回收站"
        :items="[
          `回收站位置：${eaTrash}<时间戳>/（保留原目录树，随时可原样移回）`,
          '到飞牛文件管理器打开该目录核对，确认无用后手动清空即完成删除',
          '应用不会自动删除任何数据，最终删除权在你手上',
        ]"
      />
    </UiCard>
  </template>
</template>

<style scoped>
.tabs { display: flex; gap: 8px; margin-bottom: 16px; }
.tabs button {
  border: 1px solid var(--fui-border);
  background: var(--fui-card);
  color: var(--fui-text-2);
  padding: 6px 18px;
  border-radius: var(--fui-radius-sm);
  font-size: 13px;
  cursor: pointer;
  font-family: inherit;
}
.tabs button.active {
  background: var(--fui-primary);
  border-color: var(--fui-primary);
  color: #FFF;
}

.path-row { margin: 10px 0; display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.path-row .hint { flex-basis: 100%; }
.form-label { font-size: 13px; color: var(--fui-text-2); margin-right: 8px; }
.path-input {
  border: 1px solid var(--fui-border);
  border-radius: var(--fui-radius-sm);
  padding: 6px 10px;
  font-size: 13px;
  width: 320px;
  max-width: 100%;
}
.path-input:focus { outline: none; border-color: var(--fui-primary); }
.hint { color: var(--fui-text-2); font-size: 12px; margin: 6px 0 0; }
.hint-inline { color: var(--fui-text-2); font-size: 12px; margin-left: 8px; }
.err-text { color: var(--fui-red); font-size: 13px; }

.form-grid {
  display: grid;
  grid-template-columns: 90px 1fr;
  gap: 10px 14px;
  align-items: center;
  margin-bottom: 14px;
}
.form-grid > label { font-size: 13px; color: var(--fui-text-2); }
.form-grid label.check { display: flex; align-items: center; gap: 6px; color: var(--fui-text-1); font-size: 13px; }
.pair { display: flex; align-items: center; gap: 6px; }
.pair .colon { color: var(--fui-text-2); }
.num-input {
  border: 1px solid var(--fui-border);
  border-radius: var(--fui-radius-sm);
  padding: 6px 10px;
  font-size: 13px;
  width: 100px;
}
.num-input:focus { outline: none; border-color: var(--fui-primary); }

.exec-btns { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; margin-top: 12px; }
.confirm-input {
  border: 1px solid var(--fui-border);
  border-radius: var(--fui-radius-sm);
  padding: 5px 10px;
  font-size: 13px;
  width: 200px;
  font-family: var(--fui-font-mono);
}
.confirm-input:focus { outline: none; border-color: var(--fui-primary); }

.sample { margin-top: 8px; }
.sample-list {
  background: var(--fui-panel);
  border-radius: 8px;
  padding: 10px 14px;
  font-size: 12px;
  color: var(--fui-text-2);
  max-height: 260px;
  overflow-y: auto;
  word-break: break-all;
}
</style>
