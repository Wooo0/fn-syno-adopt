<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref } from 'vue'
import { api, type AdoptedVolume, type SynoCandidate, type SystemInfo } from './api'
import UiButton from './components/ui/UiButton.vue'
import UiCard from './components/ui/UiCard.vue'
import UiBadge from './components/ui/UiBadge.vue'
import UiEmpty from './components/ui/UiEmpty.vue'
import UiSidebar, { type SidebarItem } from './components/ui/UiSidebar.vue'
import AdoptPage from './components/AdoptPage.vue'
import CleanPage from './components/CleanPage.vue'

const APP_VERSION = 'v0.5.5'

const sys = ref<SystemInfo | null>(null)
const candidates = ref<SynoCandidate[]>([])
const error = ref('')
const loading = ref(false)
const page = ref('detect')
const adopted = ref<AdoptedVolume[]>([])
const mdArrays = ref<string[]>([])
const mdMsg = ref('')
const mdBusy = ref(false)

async function scanMd() {
  mdMsg.value = ''
  try {
    const res = await api.mdScan()
    mdArrays.value = res.arrays ?? []
  } catch { mdArrays.value = [] }
}

async function assembleMd() {
  mdBusy.value = true
  mdMsg.value = ''
  try {
    const res = await api.mdAssemble()
    mdMsg.value = res.output || '组装命令已执行'
    await load()
    await scanMd()
  } catch (e) {
    mdMsg.value = e instanceof Error ? e.message : String(e)
  } finally {
    mdBusy.value = false
  }
}

const navItems: SidebarItem[] = [
  { key: 'detect', label: '检测面板' },
  { key: 'adopt', label: '磁盘接管' },
  { key: 'clean', label: '清理工具' },
]

const statusOk = computed<boolean | undefined>(() =>
  error.value ? false : sys.value ? true : undefined,
)

// 检测面板只找 vg1/volume_1，接管成功的盘改名成 trim_xxx 后会被跳过，
// 候选列表随之变空。不解释的话用户第一反应是"我的盘呢？是不是失败了"。
const adoptableCount = computed(() => candidates.value.filter((c) => c.adoptable).length)
// 注意带上 mdArrays 为空的条件：用户又接入一块新群晖盘时，候选为空但有未组装阵列，
// 此时要让位给"组装阵列"入口，而不是显示"已接管完成"。
const allAdopted = computed(
  () => candidates.value.length === 0 && mdArrays.value.length === 0 && adopted.value.length > 0,
)

async function loadAdopted() {
  try {
    const res = await api.adopted()
    adopted.value = res.volumes ?? []
  } catch {
    adopted.value = []
  }
}

// 全局运行中的任务：大卷 chown 要跑几十分钟，用户切到别的页面就看不到进度了
// （任务本身在后台继续，切回来 JobLog 会自动续轮询，这里只是补一个可见入口）
const runningJob = ref<{ kind: string; label: string; page: string } | null>(null)
let jobTimer: number | undefined

const kindLabel: Record<string, string> = {
  adopt: '磁盘接管', 'owner-repair': '所有权修复', 'eadir-clean': '@eaDir 清理', rollback: '回滚',
}
const kindPage: Record<string, string> = {
  adopt: 'adopt', rollback: 'adopt', 'owner-repair': 'clean', 'eadir-clean': 'clean',
}

async function pollRunning() {
  try {
    const res = await api.jobs()
    const live = (res.jobs ?? []).find((j) => j.status === 'running')
    runningJob.value = live
      ? { kind: live.kind, label: kindLabel[live.kind] ?? live.kind, page: kindPage[live.kind] ?? 'detect' }
      : null
  } catch {
    runningJob.value = null
  }
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [s, d] = await Promise.all([api.system(), api.disks()])
    sys.value = s.data
    candidates.value = d.candidates ?? []
    if (!candidates.value.length) await scanMd()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await load()
  await loadAdopted()
  await pollRunning()
  jobTimer = window.setInterval(pollRunning, 5000)
})
onBeforeUnmount(() => {
  if (jobTimer !== undefined) window.clearInterval(jobTimer)
})
</script>

<template>
  <div class="app">
    <UiSidebar v-model="page" :items="navItems" :version="APP_VERSION" :status-ok="statusOk">
      <template #icon="{ item }">
        <!-- 检测面板：放大镜 -->
        <svg v-if="item.key === 'detect'" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <circle cx="7" cy="7" r="4.5" /><path d="M10.5 10.5 L14 14" stroke-linecap="round" />
        </svg>
        <!-- 磁盘接管：硬盘 -->
        <svg v-else-if="item.key === 'adopt'" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <rect x="1.5" y="3.5" width="13" height="9" rx="1.5" /><path d="M4 9.5h8" stroke-linecap="round" /><circle cx="12" cy="6.5" r="0.8" fill="currentColor" stroke="none" />
        </svg>
        <!-- 清理工具：扫帚 -->
        <svg v-else viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <path d="M9.5 2.5 L13.5 6.5" stroke-linecap="round" /><path d="M8 5 L11 8 L5.5 13.5 H2.5 V10.5 Z" stroke-linejoin="round" />
        </svg>
      </template>

      <!-- 迷你流程条：告诉首次用户全貌与"我在哪" -->
      <template #flow>
        <button
          v-for="(it, i) in navItems" :key="it.key"
          class="flow-node" :class="{ 'is-active': it.key === page, 'is-done': navItems.findIndex((n) => n.key === page) > i }"
          @click="page = it.key"
        >
          <span class="flow-dot">{{ i + 1 }}</span>
          <span v-if="i < navItems.length - 1" class="flow-line"></span>
        </button>
        <div class="flow-legend">{{ navItems.find((n) => n.key === page)?.label }}</div>
      </template>

      <!-- 运行中任务：切页也能看到进度，点击跳回 -->
      <template #task>
        <button v-if="runningJob" class="task-live" @click="page = runningJob.page">
          <span class="dot"></span>{{ runningJob.label }} 进行中 · 查看
        </button>
      </template>
    </UiSidebar>

    <main class="content">
      <div v-if="error" class="banner banner-err">加载失败：{{ error }}（后端未启动？）</div>

      <template v-if="page === 'detect'">
        <UiCard title="系统信息">
          <template #actions>
            <UiBadge v-if="sys?.dryRun" type="warn">演练模式（不执行任何写操作）</UiBadge>
            <UiButton :disabled="loading" @click="load">刷新</UiButton>
          </template>
          <p v-if="sys" class="sys-line">
            <b>{{ sys.hostname }}</b>
            <span class="sep">·</span>{{ sys.os }}
            <span class="sep">·</span>内核 {{ sys.kernel }}
          </p>
        </UiCard>

        <UiCard title="疑似群晖数据盘">
          <template #desc>识别群晖数据卷（mdadm → LVM → btrfs），并区分单盘 Basic / RAID 阵列 / 跨盘卷。检测全程只读、不修改数据；当前版本只接管单盘 Basic，RAID 仅识别不接管（v0.6 支持）。</template>
          <template #actions>
            <UiBadge v-if="candidates.length" type="info">{{ candidates.length }} 个候选</UiBadge>
          </template>

          <UiEmpty v-if="candidates.length === 0 && mdArrays.length === 0 && !allAdopted">
            未发现群晖特征卷（vg1/volume_1）
            <template #hint>请确认群晖盘已接入本机，且 mdadm 阵列已组装</template>
          </UiEmpty>

          <!-- 接管成功后盘"消失"的解释：不是失败，是已经接管完成 -->
          <div v-else-if="allAdopted" class="adopted-strip">
            <p class="adopted-title">✓ 你的群晖盘已经接管完成，这里不再列出是正常现象</p>
            <p class="adopted-desc">
              接管后卷组会改名为飞牛约定的 trim_xxx 并挂载到 /volN，检测面板只找群晖原样的
              vg1/volume_1，所以候选列表为空。
            </p>
            <div v-for="v in adopted" :key="v.vgUuid" class="adopted-item mono">
              {{ v.vg }} → {{ v.mountPoint || '（未挂载）' }}
              <span v-if="v.disk">· {{ v.disk }}</span>
            </div>
            <div class="adopted-actions">
              <UiButton @click="page = 'clean'">下一步：修复文件所有权</UiButton>
              <UiButton variant="ghost" @click="page = 'adopt'">查看 / 回滚已接管卷</UiButton>
            </div>
          </div>

          <div v-else-if="candidates.length === 0" class="md-panel">
            <p class="md-title">检测到 {{ mdArrays.length }} 个群晖 mdadm 阵列特征，但尚未组装</p>
            <p class="hint">群晖阵列使用 1.2 超级块，系统不会自动组装，因此看不到卷。组装是只读性质的激活操作（不改数据），点击后自动重新检测。</p>
            <div class="md-list mono">
              <div v-for="a in mdArrays" :key="a">{{ a }}</div>
            </div>
            <p v-if="mdMsg" class="md-msg mono">{{ mdMsg }}</p>
            <UiButton :disabled="mdBusy" @click="assembleMd">
              {{ mdBusy ? '组装中…' : '组装阵列（mdadm --assemble --scan）' }}
            </UiButton>
          </div>

          <table v-else>
            <thead>
              <tr>
                <th>类型</th><th>物理盘</th><th>型号</th><th>卷</th><th>大小</th>
                <th>文件系统</th><th>状态</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="c in candidates" :key="c.dmPath">
                <tr>
                  <td>
                    <UiBadge :type="c.kind === 'basic' ? 'info' : c.kind === 'raid' ? 'ok' : 'warn'">
                      {{
                        c.kind === 'raid'
                          ? `${(c.raidLevel || 'RAID').toUpperCase()} ×${c.raidMembers || c.physDisks?.length || '?'}`
                          : c.kind === 'basic' ? '单盘 Basic' : '不支持'
                      }}
                    </UiBadge>
                  </td>
                  <td class="mono">{{ (c.physDisks?.length ?? 0) > 1 ? `${c.physDisks?.length} 块` : (c.disk || '未知') }}</td>
                  <td>{{ c.model || '—' }}</td>
                  <td class="mono">{{ c.vg }}/{{ c.lv }}</td>
                  <td>{{ c.lvSize }}</td>
                  <td>{{ c.fs || '未知' }}</td>
                  <td>
                    <UiBadge :type="c.adoptable ? 'ok' : 'warn'">
                      {{ c.adoptable ? '可接管' : '不满足' }}
                    </UiBadge>
                  </td>
                </tr>
                <tr v-if="c.kind === 'raid'">
                  <td colspan="7" class="sub">
                    阵列 {{ c.mdDev }} · {{ c.raidHealth || '未知状态' }} · 在线
                    {{ c.raidActive || '?' }}/{{ c.raidMembers || '?' }} · {{ c.raidSize || '—' }}
                    <template v-if="c.physDisks?.length">
                      <br />成员盘：{{ c.physDisks.join('、') }}
                    </template>
                  </td>
                </tr>
                <tr v-if="c.subvols?.length">
                  <td colspan="7" class="sub">子卷：{{ c.subvols.join('、') }}</td>
                </tr>
                <tr v-for="n in c.notes ?? []" :key="n">
                  <td colspan="7" class="sub warn-text">⚠ {{ n }}</td>
                </tr>
              </template>
            </tbody>
          </table>
        </UiCard>

        <!-- 有可接管盘时升级为行动条，别让用户自己猜下一步去哪 -->
        <div v-if="adoptableCount" class="next-strip">
          <span>发现 <b>{{ adoptableCount }}</b> 块可接管磁盘</span>
          <UiButton @click="page = 'adopt'">去磁盘接管</UiButton>
        </div>
        <div v-else class="info-strip">
          检测阶段不修改数据。确认候选盘无误后，到「磁盘接管」生成接管计划；接管完成后用「清理工具」修复所有权并清理 @eaDir。
        </div>
      </template>

      <!-- KeepAlive：页面切走时保留组件状态（jobId/运行视图/任务日志），
           否则 v-if 卸载后"切回来自动续轮询"不成立，侧栏"进行中·查看"也会落空 -->
      <KeepAlive>
        <AdoptPage v-if="page === 'adopt'" />
        <CleanPage v-else-if="page === 'clean'" />
      </KeepAlive>
    </main>
  </div>
</template>

<style>
body {
  margin: 0;
  height: 100vh;
  overflow: hidden;
  background: var(--fui-bg);
  color: var(--fui-text-1);
  font-family: var(--fui-font);
  font-size: 14px;
  line-height: 1.6;
}

.app { display: flex; height: 100vh; }
/* 内容区：白色圆角浮动面板，与窗口边缘留出间距（对齐飞牛原生窗口） */
.content {
  flex: 1;
  min-width: 0;
  margin: 12px 12px 12px 0;
  background: var(--fui-content);
  border-radius: var(--fui-radius);
  overflow-y: auto;
  padding: 20px 24px 32px;
}

.sys-line { margin: 0; }
.sys-line .sep { margin: 0 8px; color: var(--fui-text-disabled); }
.hint { color: var(--fui-text-2); font-size: 12px; }
.mono { font-family: var(--fui-font-mono); font-size: 13px; }

table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { text-align: left; padding: 9px 12px; border-bottom: 1px solid var(--fui-divider); }
th {
  color: var(--fui-text-2);
  font-weight: 400;
  font-size: 12px;
  background: var(--fui-panel);
}
thead tr th:first-child { border-radius: 6px 0 0 6px; }
thead tr th:last-child { border-radius: 0 6px 6px 0; }
tbody tr:last-child td { border-bottom: 0; }
.sub { color: var(--fui-text-2); font-size: 12px; }
.warn-text { color: var(--fui-yellow); }

.info-strip {
  background: var(--fui-panel);
  border-radius: 8px;
  padding: 10px 16px;
  font-size: 12px;
  color: var(--fui-text-2);
}

.md-panel {
  background: rgba(245, 157, 0, 0.06);
  border: 1px solid rgba(245, 157, 0, 0.25);
  border-radius: 8px;
  padding: 14px 16px;
}
.md-title { margin: 0 0 6px; font-weight: 600; color: var(--fui-yellow); font-size: 13px; }
.md-list {
  background: var(--fui-panel);
  border-radius: 6px;
  padding: 8px 12px;
  font-size: 12px;
  color: var(--fui-text-2);
  margin: 10px 0;
  word-break: break-all;
}
.md-msg { font-size: 12px; color: var(--fui-text-2); margin: 8px 0; white-space: pre-wrap; }

.banner {
  border-radius: 8px;
  padding: 10px 16px;
  margin-bottom: 16px;
  font-size: 13px;
}
.banner-err {
  background: rgba(220, 38, 38, 0.06);
  color: var(--fui-red);
  border: 1px solid rgba(220, 38, 38, 0.2);
}

/* 侧栏迷你流程条 / 运行中任务指示 */
.flow-node {
  display: inline-flex; align-items: center; border: 0; background: transparent;
  padding: 0; cursor: pointer; font-family: inherit;
}
.flow-dot {
  width: 16px; height: 16px; border-radius: 50%;
  background: var(--fui-grey); color: #FFF; font-size: 10px;
  display: inline-flex; align-items: center; justify-content: center;
}
.flow-node.is-done .flow-dot { background: var(--fui-green); }
.flow-node.is-active .flow-dot { background: var(--fui-primary); }
.flow-line { width: 20px; height: 2px; background: var(--fui-grey); }
.flow-node.is-done .flow-line { background: var(--fui-green); }
.flow-legend { font-size: 11px; color: var(--fui-text-2); margin-top: 4px; }
.task-live {
  border: 0; background: rgba(55, 140, 190, 0.1); color: var(--fui-blue);
  border-radius: 999px; padding: 3px 10px; font-size: 12px; cursor: pointer;
  font-family: inherit; display: inline-flex; align-items: center; gap: 6px;
}
.task-live .dot {
  width: 6px; height: 6px; border-radius: 50%; background: var(--fui-blue);
}

/* 接管完成后"盘去哪了"的解释条 */
.adopted-strip {
  background: rgba(0, 167, 87, 0.07);
  border: 1px solid rgba(0, 167, 87, 0.25);
  border-radius: 8px; padding: 14px 16px;
}
.adopted-title { margin: 0 0 6px; font-weight: 600; color: var(--fui-green); font-size: 13px; }
.adopted-desc { margin: 0; font-size: 12px; color: var(--fui-text-2); }
.adopted-item {
  background: var(--fui-panel); border-radius: 6px; padding: 6px 12px;
  font-size: 12px; color: var(--fui-text-2); margin-top: 8px; word-break: break-all;
}
.adopted-actions { display: flex; gap: 8px; margin-top: 12px; flex-wrap: wrap; }

/* 有货时的下一步行动条 */
.next-strip {
  background: var(--fui-panel); border: 1px solid var(--fui-border);
  border-radius: 8px; padding: 10px 16px; font-size: 13px; color: var(--fui-text-2);
  display: flex; align-items: center; justify-content: space-between; gap: 12px;
}
.next-strip b { color: var(--fui-primary); }
</style>
