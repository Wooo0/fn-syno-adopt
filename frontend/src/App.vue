<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, type SynoCandidate, type SystemInfo } from './api'
import UiButton from './components/ui/UiButton.vue'
import UiCard from './components/ui/UiCard.vue'
import UiBadge from './components/ui/UiBadge.vue'
import UiEmpty from './components/ui/UiEmpty.vue'
import UiSidebar, { type SidebarItem } from './components/ui/UiSidebar.vue'
import AdoptPage from './components/AdoptPage.vue'
import CleanPage from './components/CleanPage.vue'

const APP_VERSION = 'v0.5.0'

const sys = ref<SystemInfo | null>(null)
const candidates = ref<SynoCandidate[]>([])
const error = ref('')
const loading = ref(false)
const page = ref('detect')

// md 阵列自救：群晖 1.2 超级块不会被内核自动组装，候选为空时提供组装入口
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

onMounted(load)
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
          <template #desc>识别群晖 Basic 单盘卷（mdadm → LVM vg1/volume_1 → btrfs）。检测全程只读。</template>
          <template #actions>
            <UiBadge v-if="candidates.length" type="info">{{ candidates.length }} 个候选</UiBadge>
          </template>

          <UiEmpty v-if="candidates.length === 0 && mdArrays.length === 0">
            未发现群晖特征卷（vg1/volume_1）
            <template #hint>请确认群晖盘已接入本机，且 mdadm 阵列已组装</template>
          </UiEmpty>

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
                <th>物理盘</th><th>型号</th><th>卷</th><th>大小</th>
                <th>文件系统</th><th>状态</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="c in candidates" :key="c.dmPath">
                <tr>
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
                </tr>
                <tr v-if="c.subvols?.length">
                  <td colspan="6" class="sub">子卷：{{ c.subvols.join('、') }}</td>
                </tr>
                <tr v-for="n in c.notes ?? []" :key="n">
                  <td colspan="6" class="sub warn-text">⚠ {{ n }}</td>
                </tr>
              </template>
            </tbody>
          </table>
        </UiCard>

        <div class="info-strip">
          检测阶段全程只读。确认候选盘无误后，到「磁盘接管」生成接管计划；接管完成后用「清理工具」修复所有权并清理 @eaDir。
        </div>
      </template>

      <AdoptPage v-else-if="page === 'adopt'" />
      <CleanPage v-else-if="page === 'clean'" />
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
</style>
