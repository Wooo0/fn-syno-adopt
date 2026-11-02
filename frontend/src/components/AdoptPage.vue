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
// 生成计划可能很慢（每块盘都要只读挂载 + subvolume list，大卷可达数十秒），
// 没有 loading 时按钮毫无反应，用户会以为点了没生效而重复点。
const planning = ref(false)

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
  () => adoptable.value.length > 0 && adoptable.value.every((c) => selected.value.has(keyOf(c))),
)

// 选择标识：LVM 候选用 vgUuid；直建（direct）没有卷组，用 mdDev（/dev/mdN）。
// 后端按 /dev/md 前缀分流到对应接管路径。
function keyOf(c: SynoCandidate): string {
  return c.kind === 'direct' ? c.mdDev || c.dmPath : c.vgUuid
}

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
  const k = keyOf(c)
  if (s.has(k)) s.delete(k)
  else s.add(k)
  selected.value = s
}

function toggleAll() {
  selected.value = allSelected.value ? new Set() : new Set(adoptable.value.map(keyOf))
}

// 不可接管/需激活时，把「为什么」直接挂到状态徽章的悬浮提示上。
// 原因本来只在表格下方的小字里，用户常常看不见，只能干瞪眼。
function reasonOf(c: SynoCandidate): string {
  const warn = (c.notes ?? []).filter((n) => !n.startsWith('✔'))
  if (warn.length) return warn.map((n) => n.replace(/^⚠\s*/, '')).join('\n')
  return c.adoptable ? '满足接管条件' : '不满足接管条件'
}

async function makePlans() {
  if (!selected.value.size || planning.value) return
  error.value = ''
  plans.value = []
  planErrors.value = []
  // 旧 job 的日志与即将生成的新计划无关；留着它，卡片重建时 JobLog 会重挂载并把
  // 旧 job 的终态当成"新一轮完成"（见 onJobFinished 顶部的防重放说明）。
  jobId.value = null
  planning.value = true
  try {
    // 勾选的盘可能在勾选之后状态变了（卷组掉线、子卷读不到、刚被接管），
    // 这些盘会被过滤掉。绝不能静默丢弃——用户看到的只是"点了按钮什么都没发生"。
    const valid = new Set(adoptable.value.map(keyOf))
    const ids: string[] = []
    const skipped: string[] = []
    for (const k of selected.value) {
      if (valid.has(k)) {
        ids.push(k)
        continue
      }
      const c = candidates.value.find((x) => keyOf(x) === k)
      const why = c?.adopted
        ? '已由本工具接管'
        : (c?.notes ?? []).find((n) => !n.startsWith('✔'))?.replace(/^⚠\s*/, '') ?? '当前不满足接管条件'
      skipped.push(`${c?.disk || k}（${why}）`)
    }
    if (!ids.length) {
      error.value = `所选磁盘当前都无法生成接管计划：${skipped.join('；')}。` +
        '请点「刷新检测」后查看该行「状态」列与下方说明；若是「需激活」请先点『激活卷组』。'
      return
    }
    const res = await api.adoptPlan(ids)
    plans.value = res.plans ?? []
    planErrors.value = res.errors ?? []
    if (skipped.length) {
      error.value = `以下磁盘已跳过（当前不可接管）：${skipped.join('；')}`
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    planning.value = false
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
    // direct 计划用 mdDev 定位，LVM 计划用 vgUuid
    const id = p.kind === 'direct' ? p.mdDev || p.dmPath : p.vgUuid
    const res = await api.adoptExec(id, runMode.value === 'dry', runMode.value === 'real' ? 'ADOPT' : '')
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
  // 防事件重放（真机 2026-10-31 踩坑，症状极具迷惑性）：
  // 接管成功后会清空 plans → 第 2 步卡片 v-if 变假 → JobLog 被卸载；用户下一次点
  // 「生成接管计划」，卡片重现 → JobLog 重新挂载，它 watch(jobId, immediate) 会立刻
  // 再轮询**上一轮**的 jobId。那个 job 早就是终态，于是再次 emit finished → 这里
  // 把刚生成的新计划和新勾选一并清掉。用户视角：点按钮后卡片闪一下、勾选全没、
  // 一个字报错都没有。队列已不在跑（running=false）时收到 finished，一律视为重放。
  if (!running.value) return
  if (job && job.status === 'error') {
    running.value = false
    runQueue.value = []
    // 先刷新再设错误：load() 会清 error，顺序反了报错就"闪一下没了"（真机教训：
    // 用户还没看清错误内容它就消失了，只能对着已完成的接管猜哪步出的错）
    await load()
    error.value = `第 ${runIndex.value} / ${runTotal.value} 块盘接管失败：${job.error ?? '未知错误'}。队列已中止，请先处理该盘（可用下方「已接管卷 / 回滚」）。`
    return
  }
  adoptDonePoint.value = runQueue.value[0]?.mountPoint ?? adoptDonePoint.value
  finished.value++
  runQueue.value.shift()
  if (runQueue.value.length) {
    runQueueStep()
  } else {
    running.value = false
    adoptDone.value = runMode.value === 'real'
    dryDone.value = runMode.value === 'dry'
    if (runMode.value === 'real') {
      // 计划已消费（挂载点/卷名均已生效），清空防止用旧计划重复执行。
      // 演练不清空：不写盘、计划可复用，保留卡片供用户核对步骤后直接正式执行。
      plans.value = []
      confirmText.value = ''
      selected.value = new Set()
    }
    await Promise.all([load(), loadAdopted()]) // 完成后刷新检测与接管记录
  }
}

function startRun(mode: 'dry' | 'real') {
  if (!plans.value.length || running.value) return
  if (mode === 'real' && !canRealRun.value) return
  runMode.value = mode
  jobId.value = null // 新一轮从干净日志开始，别让上一轮的终态被当成本轮结果
  adoptDone.value = false
  dryDone.value = false
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
  rbJobId.value = null
  try {
    const res = await api.adoptRollback(v.vgUuid, dryRun, dryRun ? '' : 'ROLLBACK')
    rbJobId.value = res.jobId
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    rbRunning.value = false
  }
}

// 批量回滚：接管是「一次选多块、串行执行」，回滚也应当能一次处理完，
// 否则多盘场景下得挨个点、还容易漏（真机 2026-10-31 用户提出的正是这一点）。
const rollbackableCount = computed(() => adopted.value.filter((v) => v.rollbackable).length)

async function runRollbackAll(dryRun: boolean) {
  error.value = ''
  rbRunning.value = true
  rbJobId.value = null
  try {
    const res = await api.adoptRollbackAll(dryRun, dryRun ? '' : 'ROLLBACK')
    rbJobId.value = res.jobId
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    rbRunning.value = false
  }
}

function onRollbackJobDone() {
  // 同 onJobFinished：JobLog 重挂载会重放旧 jobId 的终态，非本次发起的一律忽略。
  if (!rbRunning.value) return
  rbRunning.value = false
  rbConfirm.value = ''
  loadAdopted()
  load()
}

// 接管成功的收尾指引：只认 kind='adopt' 且终态 done，
// rollback / 演练都不触发（演练时 job 也是 done，故额外排除 dry 模式）
const adoptDone = ref(false)
const adoptDonePoint = ref('')
// 演练完成提示：演练不写盘、计划保留，提示用户核对步骤后可直接正式执行
const dryDone = ref(false)

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
  <!-- 本页错误必须可见：之前只往 ref 里写、模板从不渲染，失败时界面纹丝不动，
       用户只看到"按钮闪了一下、勾选没了"，完全无从排查（真机教训）。 -->
  <div v-if="error" class="banner banner-err">
    <span class="banner-text">{{ error }}</span>
    <button class="banner-close" title="关闭" @click="error = ''">×</button>
  </div>

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
          <!-- 直建 btrfs 没有 vgUuid，用 dmPath 做 key 以免两行撞成一行 -->
          <template v-for="c in candidates" :key="c.vgUuid || c.dmPath">
            <tr>
              <td>
                <!-- 已勾选的盘即使刚变成"不可接管"也保留勾选框（可手动取消）：
                     勾选框凭空消失会让用户以为"我的勾选被系统偷偷取消了"。 -->
                <input
                  v-if="c.adoptable || selected.has(keyOf(c))" type="checkbox"
                  :checked="selected.has(keyOf(c))" @change="toggle(c)"
                />
              </td>
              <td>
                <UiBadge
                  :type="
                    c.kind === 'basic' ? 'info'
                      : c.kind === 'raid' ? 'ok'
                        : c.kind === 'direct' ? 'info' : 'warn'
                  "
                >
                  {{
                    c.kind === 'raid'
                      ? `${(c.raidLevel || 'RAID').toUpperCase()} ×${c.raidMembers || '?'}`
                      : c.kind === 'basic' ? '单盘 Basic'
                        : c.kind === 'direct' ? '直建 btrfs' : '不支持'
                  }}
                </UiBadge>
              </td>
              <td class="mono">{{ c.disk || '未知' }}</td>
              <td>{{ c.model || '—' }}</td>
              <td class="mono">{{ c.kind === 'direct' ? c.mdDev || c.dmPath : `${c.vg}/${c.lv}` }}</td>
              <td>{{ c.lvSize }}</td>
              <td>{{ c.fs || '未知' }}</td>
              <td>
                    <UiBadge v-if="c.adoptable" type="ok">可接管</UiBadge>
                    <UiBadge v-else-if="c.adopted" type="info" :title="reasonOf(c)">已接管</UiBadge>
                    <UiBadge v-else-if="c.devReady === false" type="err" :title="reasonOf(c)">需激活</UiBadge>
                    <UiBadge v-else type="warn" :title="reasonOf(c)">不满足</UiBadge>
              </td>
              <td>
                <!-- 未激活就用不了（读不到文件系统与子卷）：不管是否可接管都给激活入口。
                     用 devReady 判断而非 lvActive：lvs 的激活位在真机上未必可信 -->
                <UiButton
                  v-if="c.devReady === false" variant="ghost"
                  :disabled="running" @click="activate(c)"
                >激活卷组</UiButton>
              </td>
            </tr>
            <tr v-if="c.subvols?.length">
              <td colspan="9" class="sub">子卷：{{ c.subvols.join('、') }}</td>
            </tr>
            <!-- 以 ✔ 开头的是说明性信息（如读取流程说明），不按告警渲染 -->
            <tr v-for="n in c.notes ?? []" :key="n">
              <td v-if="n.startsWith('✔')" colspan="9" class="sub">{{ n }}</td>
              <td v-else colspan="9" class="sub warn-text">⚠ {{ n }}</td>
            </tr>
          </template>
        </tbody>
      </table>

      <div v-if="adoptable.length || selected.size" class="row-actions">
        <label class="check-all">
          <input type="checkbox" :checked="allSelected" @change="toggleAll" /> 全选可接管磁盘
        </label>
        <UiButton :disabled="!selected.size || running || planning" @click="makePlans">
          {{ planning ? '正在生成计划…' : '生成接管计划' }}
        </UiButton>
        <span v-if="planning" class="planning-hint">
          每块盘需只读挂载并列出子卷，大容量盘可能需要几十秒，请勿重复点击
        </span>
      </div>
    </template>
  </UiCard>

  <!-- v-if 必须包含 planErrors：计划全部失败时 plans 为空，若只看 plans.length，
       失败原因会被整卡吞掉——用户只看到"闪一下没了"，无从排查（真机教训） -->
  <UiCard v-if="plans.length || planErrors.length" title="第 2 步 · 核对接管计划">
    <template #desc>
      数据零拷贝：LVM 布局仅重命名卷组（vg1→trim_随机串 / volume_1→0）；直建布局（无 LVM）不改任何卷名。
      之后统一切换默认子卷到 @syno、挂载并注册到飞牛存储表。执行前自动完成配置与 mount 表双备份。
    </template>
    <div v-for="p in plans" :key="p.vgUuid" class="plan">
      <div class="plan-head">
        <b class="mono">{{ p.disk || (p.kind === 'direct' ? p.mdDev : p.oldVG) }}</b>
        <span class="sep">·</span>{{ p.lvSize }}
        <UiBadge v-if="p.kind === 'direct'" type="info" style="margin-left: 4px">直建 btrfs</UiBadge>
        <span class="arrow">→</span>
        <UiBadge type="info">{{ p.mountPoint }}</UiBadge>
        <template v-if="p.kind === 'direct'">
          <span class="plan-meta">阵列 <span class="mono">{{ p.mdDev }}</span>（不改名）</span>
        </template>
        <template v-else>
          <span class="plan-meta">新卷组 <span class="mono">{{ p.newVG }}</span></span>
          <span class="plan-meta">逻辑卷 <span class="mono">{{ p.oldLV }} → 0</span></span>
        </template>
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
      <p class="hint">正式接管会改写 LVM 元数据（仅 LVM 布局）与飞牛存储注册表（数据一律不动）。请确保已对核心数据做过备份，且群晖系统已关机。</p>
      <p v-if="dryDone" class="hint dry-done">
        ✅ <strong>演练完成，未写盘</strong>——上方任务日志是每一步的实际执行情况（跳过的步骤会标注「已满足，跳过」）。
        核对无误后输入 ADOPT 即可正式接管；计划仍保留，可反复演练。
      </p>
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
      接管中途失败留下的「半接管」盘会以 <UiBadge type="err">接管中断</UiBadge> 标出，可一键还原回群晖原样。
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
              <td class="mono">
                {{ v.vg }}/{{ v.lv }}
                <span v-if="v.orphan && v.oldVG" class="restore">
                  → 还原为 {{ v.oldVG }}/{{ v.oldLV || '?' }}
                </span>
              </td>
              <td class="mono">{{ v.disk || '—' }}</td>
              <td>{{ v.lvSize }}</td>
              <td class="mono">{{ v.mountPoint || '—' }}</td>
              <td>
                <UiBadge :type="v.mounted ? 'ok' : 'info'">{{ v.mounted ? '已挂载' : '未挂载' }}</UiBadge>
              </td>
              <td>
                <UiBadge v-if="v.orphan" type="err">接管中断</UiBadge>
                <UiBadge v-else :type="v.registered ? 'ok' : 'warn'">{{ v.registered ? '已注册' : '未注册' }}</UiBadge>
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
        <template v-if="rollbackableCount > 1">
          <UiButton variant="default" :disabled="rbRunning" @click="runRollbackAll(true)">
            演练全部（{{ rollbackableCount }} 块）
          </UiButton>
          <UiButton
            variant="danger"
            :disabled="rbRunning || rbConfirm.trim().toUpperCase() !== 'ROLLBACK'"
            @click="runRollbackAll(false)"
          >全部回滚（{{ rollbackableCount }} 块）</UiButton>
        </template>
      </div>
      <p v-if="rollbackableCount > 1" class="hint">
        全部回滚按列表顺序逐块串行执行，任一块失败立即中止、后面的盘不动。
        多块群晖盘的原名都叫 vg1 时，LVM 不允许两个同名卷组——第二块会因「原名被占用」停下，
        先回滚的那块保持原名即可。
      </p>
      <p class="hint">
        回滚 = 卸载 /volN → 卷组名改回 vg1 → 逻辑卷名改回 volume_1 → 删除 mount 表注册（数据一律不动）。
        直建布局（卷组显示为 /dev/mdN）没有卷组名，回滚仅卸载并撤销注册。
        若提示「原名被其它卷组占用」，是因为整卡直通下多块群晖盘都叫 vg1，请先回滚占用该名的那块盘。
      </p>
      <JobLog :job-id="rbJobId" @finished="onRollbackJobDone" />
    </template>
  </UiCard>
</template>

<style scoped>
.banner {
  display: flex; align-items: flex-start; gap: 10px;
  border-radius: 8px; padding: 10px 14px; margin-bottom: 16px;
  font-size: 13px; line-height: 1.6;
}
.banner-err {
  background: rgba(220, 38, 38, 0.06);
  color: var(--fui-red);
  border: 1px solid rgba(220, 38, 38, 0.2);
}
.banner-text { flex: 1; word-break: break-word; white-space: pre-wrap; }
.banner-close {
  border: 0; background: transparent; color: inherit; cursor: pointer;
  font-size: 16px; line-height: 1; padding: 0 2px; opacity: 0.6; font-family: inherit;
}
.banner-close:hover { opacity: 1; }

.planning-hint { font-size: 12px; color: var(--fui-text-2); }

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
.restore { color: var(--fui-red); font-size: 12px; margin-left: 4px; }
</style>
