<script setup lang="ts">
import { computed, onActivated, onMounted, ref } from 'vue'
import { api, type AdoptPlan, type AdoptedVolume, type Job, type SynoCandidate } from '../api'
import UiButton from './ui/UiButton.vue'
import UiCard from './ui/UiCard.vue'
import UiBadge from './ui/UiBadge.vue'
import UiEmpty from './ui/UiEmpty.vue'
import JobLog from './JobLog.vue'
import DoneCard from './DoneCard.vue'

const candidates = ref<SynoCandidate[]>([])
const loading = ref(false)
const error = ref('')
const selected = ref<Set<string>>(new Set()) // 按 vgUuid 选择
const plans = ref<AdoptPlan[]>([])
const planErrors = ref<{ vgUuid: string; error: string }[]>([])

const jobId = ref<string | null>(null)
const running = ref(false)
const runMode = ref<'' | 'dry' | 'real'>('')
const runQueue = ref<AdoptPlan[]>([])
const runIndex = ref(0)
const runTotal = ref(0)
const confirmText = ref('')
const finished = ref(0)

const adoptable = computed(() => candidates.value.filter((c) => c.adoptable))
const allSelected = computed(
  () => adoptable.value.length > 0 && adoptable.value.every((c) => selected.value.has(c.vgUuid)),
)

async function load(keepSelection = true) {
  loading.value = true
  error.value = ''
  try {
    const d = await api.disks()
    candidates.value = d.candidates ?? []
    if (!keepSelection) selected.value = new Set()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

function toggle(c: SynoCandidate) {
  const s = new Set(selected.value)
  if (s.has(c.vgUuid)) s.delete(c.vgUuid)
  else s.add(c.vgUuid)
  selected.value = s
}

function toggleAll() {
  selected.value = allSelected.value ? new Set() : new Set(adoptable.value.map((c) => c.vgUuid))
}

async function makePlans() {
  if (!selected.value.size) return
  error.value = ''
  plans.value = []
  planErrors.value = []
  try {
    const res = await api.adoptPlan([...selected.value])
    plans.value = res.plans ?? []
    planErrors.value = res.errors ?? []
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

// —— 顺序执行队列：逐盘接管，避免并发动盘 ——
const canRealRun = computed(() => confirmText.value.trim().toUpperCase() === 'ADOPT')

async function runQueueStep() {
  if (!runQueue.value.length) {
    running.value = false
    return
  }
  const p = runQueue.value[0]
  runIndex.value = runTotal.value - runQueue.value.length + 1
  try {
    const res = await api.adoptExec(p.vgUuid, runMode.value === 'dry', runMode.value === 'real' ? 'ADOPT' : '')
    jobId.value = res.jobId
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    running.value = false
    runQueue.value = []
  }
}

// 多盘顺序接管：任一块盘失败立即中止队列，不再继续后面的盘
//（否则前一块已改名、后一块继续跑，会留下多盘半接管状态难以收拾）
async function onJobFinished(job: Job | null) {
  if (job && job.status === 'error') {
    error.value = `第 ${runIndex.value} / ${runTotal.value} 块盘接管失败：${job.error ?? '未知错误'}。队列已中止，请先处理该盘（可用下方「已接管卷 / 回滚」）。`
    running.value = false
    runQueue.value = []
    load() // 刷新检测，让已改名的盘状态可见
    return
  }
  adoptDonePoint.value = runQueue.value[0]?.mountPoint ?? adoptDonePoint.value
  finished.value++
  runQueue.value.shift()
  if (runQueue.value.length) {
    runQueueStep()
  } else {
    running.value = false
    // 收尾卡片：只在整队跑完且是正式接管时出现（演练/回滚/修复都不触发）
    adoptDone.value = runMode.value === 'real'
    // 计划已消费（挂载点/卷名均已生效），清空防止用旧计划重复执行
    plans.value = []
    confirmText.value = ''
    selected.value = new Set()
    await Promise.all([load(), loadAdopted()]) // 完成后刷新检测与接管记录
  }
}

function startRun(mode: 'dry' | 'real') {
  if (!plans.value.length || running.value) return
  if (mode === 'real' && !canRealRun.value) return
  runMode.value = mode
  adoptDone.value = false
  adoptDonePoint.value = ''
  runQueue.value = [...plans.value]
  runTotal.value = plans.value.length
  runIndex.value = 0
  finished.value = 0
  running.value = true
  error.value = ''
  runQueueStep()
}

// —— 已接管卷 / 回滚 ——
const adopted = ref<AdoptedVolume[]>([])
const rbConfirm = ref('')
const rbJobId = ref<string | null>(null)
const rbRunning = ref(false)

async function loadAdopted() {
  try {
    const res = await api.adopted()
    adopted.value = res.volumes ?? []
  } catch {
    adopted.value = []
  }
}

async function runRollback(v: AdoptedVolume, dryRun: boolean) {
  error.value = ''
  rbRunning.value = true
  try {
    const res = await api.adoptRollback(v.vgUuid, dryRun, dryRun ? '' : 'ROLLBACK')
    rbJobId.value = res.jobId
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    rbRunning.value = false
  }
}

function onRollbackJobDone() {
  rbRunning.value = false
  rbConfirm.value = ''
  loadAdopted()
  load()
}

// 接管成功的收尾指引：只认 kind='adopt' 且终态 done，
// rollback / 演练都不触发（演练时 job 也是 done，故额外排除 dry 模式）
const adoptDone = ref(false)
const adoptDonePoint = ref('')

async function activate(c: SynoCandidate) {
  error.value = ''
  try {
    await api.lvmActivate(c.vgUuid)
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

onMounted(async () => {
  await load(false)
  await loadAdopted()
})

// KeepAlive 缓存后 onMounted 只在首次挂载跑一次。切回本页必须主动刷新，
// 否则会看到缓存时的旧候选列表——最典型的场景：用户在检测页点「组装阵列」
// 后经行动条跳进来，若没有这个钩子，看到的是"未发现群晖候选盘"的空白页。
// 只在空闲时刷新，避免与队列推进时的刷新打架；勾选与作业状态均保留。
// 注意：KeepAlive 内首次挂载也会触发 onActivated（在 onMounted 之后），
// 这里跳过首次，避免和 onMounted 重复拉一次数据。
let activatedOnce = false
onActivated(() => {
  if (!activatedOnce) {
    activatedOnce = true
    return
  }
  if (running.value || rbRunning.value) return
  load()
  loadAdopted()
})
</script>

<template>
  <UiCard title="第 1 步 · 选择要接管的磁盘">
    <template #desc>
      列出来自检测面板的群晖候选盘。多块盘卷组重名（都叫 vg1），已按卷组 UUID 区分，选择与执行均以 UUID 定位。
    </template>
    <template #actions>
      <UiBadge v-if="adoptable.length" type="info">{{ adoptable.length }} 块可接管</UiBadge>
      <UiButton variant="ghost" :disabled="loading" @click="load()">刷新检测</UiButton>
    </template>

    <UiEmpty v-if="candidates.length === 0">
      未发现群晖候选盘
      <template #hint>请确认 HBA 卡已直通给飞牛，且磁盘已接入</template>
    </UiEmpty>

    <template v-else>
      <table>
        <thead>
          <tr>
            <th style="width: 36px"></th>
            <th>类型</th>
            <th>物理盘</th><th>型号</th><th>卷组</th><th>大小</th>
            <th>文件系统</th><th>状态</th><th>操作</th>
          </tr>
        </thead>
        <tbody>
          <template v-for="c in candidates" :key="c.vgUuid">
            <tr>
              <td>
                <input
                  v-if="c.adoptable" type="checkbox"
                  :checked="selected.has(c.vgUuid)" @change="toggle(c)"
                />
              </td>
              <td>
                <UiBadge :type="c.kind === 'basic' ? 'info' : c.kind === 'raid' ? 'ok' : 'warn'">
                  {{
                    c.kind === 'raid'
                      ? `${(c.raidLevel || 'RAID').toUpperCase()} ×${c.raidMembers || '?'}`
                      : c.kind === 'basic' ? '单盘 Basic' : '不支持'
                  }}
                </UiBadge>
              </td>
              <td class="mono">{{ c.disk || '未知' }}</td>
              <td>{{ c.model || '—' }}</td>
              <td class="mono">{{ c.vg }}/{{ c.lv }}</td>
              <td>{{ c.lvSize }}</td>
              <td>{{ c.fs || '未知' }}</td>
              <td>
                <UiBadge :type="c.adoptable ? 'ok' : 'warn'">
                  {{ c.adoptable ? '可接管' : '不满足' }}
                </UiBadge>
              </td>
              <td>
                <!-- 未激活就用不了（读不到文件系统与子卷）：不管是否可接管都给激活入口 -->
                <UiButton
                  v-if="!c.lvActive" variant="ghost"
                  :disabled="running" @click="activate(c)"
                >激活卷组</UiButton>
              </td>
            </tr>
            <tr v-if="c.subvols?.length">
              <td colspan="9" class="sub">子卷：{{ c.subvols.join('、') }}</td>
            </tr>
            <tr v-for="n in c.notes ?? []" :key="n">
              <td colspan="9" class="sub warn-text">⚠ {{ n }}</td>
            </tr>
          </template>
        </tbody>
      </table>

      <div v-if="adoptable.length" class="row-actions">
        <label class="check-all">
          <input type="checkbox" :checked="allSelected" @change="toggleAll" /> 全选可接管磁盘
        </label>
        <UiButton :disabled="!selected.size || running" @click="makePlans">生成接管计划</UiButton>
      </div>
    </template>
  </UiCard>

  <UiCard v-if="plans.length" title="第 2 步 · 核对接管计划">
    <template #desc>
      数据零拷贝：仅重命名 LVM（vg1→trim_随机串 / volume_1→0）、切换默认子卷到 @syno、挂载并注册到飞牛存储表。
      执行前自动完成 vgcfgbackup 与 mount 表双备份。
    </template>
    <div v-for="p in plans" :key="p.vgUuid" class="plan">
      <div class="plan-head">
        <b class="mono">{{ p.disk || p.oldVG }}</b>
        <span class="sep">·</span>{{ p.lvSize }}
        <span class="arrow">→</span>
        <UiBadge type="info">{{ p.mountPoint }}</UiBadge>
        <span class="plan-meta">新卷组 <span class="mono">{{ p.newVG }}</span></span>
        <span class="plan-meta">逻辑卷 <span class="mono">{{ p.oldLV }} → 0</span></span>
        <span class="plan-meta">mount.uuid <span class="mono">{{ p.muuid }}</span></span>
      </div>
      <p v-for="w in p.warnings ?? []" :key="w" class="sub warn-text">⚠ {{ w }}</p>
      <ol class="plan-steps">
        <li v-for="(s, i) in p.steps" :key="i">{{ s }}</li>
      </ol>
    </div>
    <p v-for="e in planErrors" :key="e.vgUuid" class="sub warn-text">✗ {{ e.vgUuid }}: {{ e.error }}</p>

    <div class="exec">
      <div v-if="running" class="progress">
        正在处理第 {{ runIndex }} / {{ runTotal }} 块盘（已完成 {{ finished }}）
      </div>
      <div class="exec-btns">
        <UiButton variant="default" :disabled="running" @click="startRun('dry')">
          演练执行（不写盘）
        </UiButton>
        <UiButton
          variant="danger" :disabled="running || !canRealRun"
          title="输入 ADOPT 后可用"
          @click="startRun('real')"
        >
          正式接管（{{ plans.length }} 块盘）
        </UiButton>
        <input
          v-model="confirmText" class="confirm-input mono" :disabled="running"
          placeholder="输入 ADOPT 解锁正式执行" spellcheck="false"
        />
      </div>
      <p class="hint">正式接管会改写 LVM 元数据与飞牛存储注册表（数据不动）。请确保已对核心数据做过备份，且群晖系统已关机。</p>
      <JobLog :job-id="jobId" @finished="onJobFinished" />
      <DoneCard
        v-if="adoptDone"
        title="接管完成"
        :items="[
          `去飞牛「存储管理器」确认新存储空间${adoptDonePoint ? '（' + adoptDonePoint + '）' : ''}；未显示就刷新页面或重启系统`,
          '打开「清理工具 → 所有权修复」，把群晖遗留 uid 改成你的飞牛用户（否则文件可能读写不了）',
          '再执行「@eaDir 清理」，清掉群晖的缩略图缓存目录',
          '万一不对：下方「已接管卷 / 回滚」可一键还原回群晖原样',
        ]"
      />
    </div>
  </UiCard>

  <UiCard title="已接管卷 · 回滚">
    <template #desc>
      仅列出<strong>本应用接管过</strong>的卷（凭据：<span class="mono">/var/lib/fn-syno-adopt/adopted.json</span>）——
      飞牛原生存储空间同样叫 trim_xxx/0，绝不列入，避免误回滚你自己的存储池。
      接管中途失败留下的「半接管」盘会在这里出现，可一键还原回群晖原样。
    </template>
    <template #actions>
      <UiButton variant="ghost" :disabled="running || rbRunning" @click="loadAdopted">刷新</UiButton>
    </template>

    <UiEmpty v-if="adopted.length === 0">
      暂无接管记录
      <template #hint>接管成功后这里会出现可回滚的卷</template>
    </UiEmpty>

    <template v-else>
      <table>
        <thead>
          <tr>
            <th>卷组</th><th>物理盘</th><th>大小</th><th>挂载点</th>
            <th>挂载</th><th>已注册</th><th>操作</th>
          </tr>
        </thead>
        <tbody>
          <template v-for="v in adopted" :key="v.vgUuid">
            <tr>
              <td class="mono">{{ v.vg }}/{{ v.lv }}</td>
              <td class="mono">{{ v.disk || '—' }}</td>
              <td>{{ v.lvSize }}</td>
              <td class="mono">{{ v.mountPoint || '—' }}</td>
              <td>
                <UiBadge :type="v.mounted ? 'ok' : 'info'">{{ v.mounted ? '已挂载' : '未挂载' }}</UiBadge>
              </td>
              <td>
                <UiBadge :type="v.registered ? 'ok' : 'warn'">{{ v.registered ? '已注册' : '未注册' }}</UiBadge>
              </td>
              <td>
                <div class="rb-btns">
                  <UiButton variant="ghost" :disabled="rbRunning || !v.rollbackable" @click="runRollback(v, true)">
                    演练
                  </UiButton>
                  <UiButton
                    variant="danger" :disabled="rbRunning || !v.rollbackable || rbConfirm.trim().toUpperCase() !== 'ROLLBACK'"
                    @click="runRollback(v, false)"
                  >回滚</UiButton>
                </div>
              </td>
            </tr>
            <tr v-for="n in v.notes ?? []" :key="n">
              <td colspan="7" class="sub warn-text">⚠ {{ n }}</td>
            </tr>
          </template>
        </tbody>
      </table>

      <div class="exec-btns">
        <input
          v-model="rbConfirm" class="confirm-input mono" :disabled="rbRunning"
          placeholder="输入 ROLLBACK 解锁回滚" spellcheck="false"
        />
      </div>
      <p class="hint">
        回滚 = 卸载 /volN → 卷组名改回 vg1 → 逻辑卷名改回 volume_1 → 删除 mount 表注册（数据一律不动）。
        若提示「原名被其它卷组占用」，是因为整卡直通下多块群晖盘都叫 vg1，请先回滚占用该名的那块盘。
      </p>
      <JobLog :job-id="rbJobId" @finished="onRollbackJobDone" />
    </template>
  </UiCard>
</template>

<style scoped>
.row-actions {
  display: flex; align-items: center; gap: 16px; margin-top: 12px;
}
.check-all { font-size: 13px; color: var(--fui-text-2); display: flex; align-items: center; gap: 6px; }

.plan {
  border: 1px solid var(--fui-border);
  border-radius: 8px;
  padding: 12px 16px;
  margin-bottom: 12px;
}
.plan-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.plan-head .sep { color: var(--fui-text-disabled); }
.plan-head .arrow { color: var(--fui-primary); font-weight: 600; }
.plan-meta { font-size: 12px; color: var(--fui-text-2); }
.plan-steps {
  margin: 8px 0 0;
  padding-left: 20px;
  font-size: 12px;
  color: var(--fui-text-2);
}
.plan-steps li { margin: 2px 0; }

.exec { margin-top: 16px; padding-top: 14px; border-top: 1px solid var(--fui-divider); }
.progress { font-size: 13px; color: var(--fui-blue); margin-bottom: 10px; font-weight: 600; }
.exec-btns { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.confirm-input {
  border: 1px solid var(--fui-border);
  border-radius: var(--fui-radius-sm);
  padding: 5px 10px;
  font-size: 13px;
  width: 220px;
  font-family: var(--fui-font-mono);
}
.confirm-input:focus { outline: none; border-color: var(--fui-primary); }
.exec .hint { color: var(--fui-text-2); font-size: 12px; margin: 10px 0 0; }
.rb-btns { display: flex; gap: 6px; }
</style>
