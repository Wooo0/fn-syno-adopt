<script setup lang="ts">
// 任务历史：终态任务日志已落盘（/var/lib/fn-syno-adopt/jobs/<id>.json），
// 应用重启、页面刷新后仍可回看——根治"报错闪一下就没，事后无法查证"。
import { onActivated, onMounted, ref } from 'vue'
import { api, type Job } from '../api'
import UiBadge from './ui/UiBadge.vue'
import UiButton from './ui/UiButton.vue'
import UiEmpty from './ui/UiEmpty.vue'

const jobs = ref<Job[]>([])
const loading = ref(false)
const expanded = ref<string>('') // 当前展开详情的任务 ID
const error = ref('')

const kindText: Record<string, string> = {
  adopt: '磁盘接管',
  'owner-repair': '所有权修复',
  'eadir-clean': '@eaDir 清理',
  rollback: '回滚',
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.jobs()
    jobs.value = res.jobs ?? []
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

async function remove(id: string) {
  try {
    await api.jobDelete(id)
    jobs.value = jobs.value.filter((j) => j.id !== id)
    if (expanded.value === id) expanded.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

function toggle(id: string) {
  expanded.value = expanded.value === id ? '' : id
}

function fmtTime(t?: string): string {
  if (!t) return '—'
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}

onMounted(load)
onActivated(load)
</script>

<template>
  <UiCard title="任务历史">
    <template #desc>
      全部写操作（接管 / 回滚 / 修复 / 清理）的终态日志，已持久化到
      <span class="mono">/var/lib/fn-syno-adopt/jobs/</span>——
      应用重启、页面刷新后仍可回看每一步的执行结果与报错详情。
      系统同时只允许一个写任务在跑；历史任务仅供参考，不能重放。
    </template>
    <template #actions>
      <UiBadge v-if="jobs.length" type="info">{{ jobs.length }} 条</UiBadge>
      <UiButton :disabled="loading" @click="load">{{ loading ? '加载中…' : '刷新' }}</UiButton>
    </template>

    <p v-if="error" class="err-text">✗ {{ error }}</p>

    <UiEmpty v-if="!loading && jobs.length === 0">
      还没有任务记录
      <template #hint>执行接管 / 修复 / 清理后，这里会出现可回看的任务日志</template>
    </UiEmpty>

    <div v-for="j in jobs" :key="j.id" class="task">
      <div class="task-head" @click="toggle(j.id)">
        <UiBadge :type="j.status === 'done' ? 'ok' : j.status === 'error' ? 'err' : 'info'">
          {{ j.status === 'done' ? '✓ 完成' : j.status === 'error' ? '✗ 失败' : '执行中' }}
        </UiBadge>
        <span class="task-kind">{{ kindText[j.kind] || j.kind }}</span>
        <span class="task-time">{{ fmtTime(j.start) }}</span>
        <span v-if="j.status === 'error' && j.error" class="task-err mono">{{ j.error }}</span>
        <span class="task-id mono">{{ j.id }}</span>
        <span class="spacer" />
        <UiButton variant="ghost" @click.stop="remove(j.id)">删除</UiButton>
      </div>
      <ol v-if="expanded === j.id" class="steps">
        <li v-for="(s, i) in j.steps ?? []" :key="i" :class="`st-${s.status}`">
          <div class="st-line">
            <span class="st-mark">{{ { ok: '✓', fail: '✗', skip: '–', pending: '…', warn: '⚠' }[s.status] || '·' }}</span>
            <span class="st-name">{{ s.name }}</span>
            <span class="st-status">{{ { ok: '成功', fail: '失败', skip: '跳过', pending: '…', warn: '警告' }[s.status] || s.status }}</span>
          </div>
          <div v-if="s.command" class="st-cmd mono">{{ s.command }}</div>
          <div v-if="s.detail" class="st-detail">{{ s.detail }}</div>
        </li>
      </ol>
    </div>
  </UiCard>
</template>

<style scoped>
.task {
  border: 1px solid var(--fui-border);
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 8px;
}
.task-head {
  display: flex;
  align-items: center;
  gap: 10px;
  cursor: pointer;
  flex-wrap: wrap;
}
.task-kind { font-size: 13px; color: var(--fui-text-1); font-weight: 600; }
.task-time { font-size: 12px; color: var(--fui-text-2); }
.task-err {
  font-size: 12px;
  color: var(--fui-red);
  word-break: break-all;
  max-width: 40%;
}
.task-id { font-size: 12px; color: var(--fui-text-2); margin-left: auto; }
.spacer { flex: 1; }

.steps { list-style: none; margin: 10px 0 0; padding: 0; }
.steps li {
  padding: 7px 10px;
  border-radius: 6px;
  margin-bottom: 4px;
  background: var(--fui-panel);
}
.steps li.st-fail { background: var(--fui-danger-bg); }
.steps li.st-warn { background: rgba(245, 157, 0, 0.08); }
.st-warn .st-mark { color: var(--fui-yellow); }
.st-warn .st-detail { color: var(--fui-yellow); }
.st-line { display: flex; align-items: baseline; gap: 8px; }
.st-mark { width: 14px; text-align: center; flex-shrink: 0; }
.st-ok .st-mark { color: var(--fui-green); }
.st-fail .st-mark { color: var(--fui-red); }
.st-skip .st-mark { color: var(--fui-text-2); }
.st-pending .st-mark { color: var(--fui-blue); }
.st-name { font-size: 13px; color: var(--fui-text-1); }
.st-status { margin-left: auto; font-size: 12px; color: var(--fui-text-2); }
.st-cmd {
  margin: 3px 0 0 22px;
  font-size: 12px;
  color: var(--fui-text-2);
  word-break: break-all;
}
.st-detail {
  margin: 2px 0 0 22px;
  font-size: 12px;
  color: var(--fui-text-2);
  white-space: pre-wrap;
  word-break: break-all;
}
.st-fail .st-detail { color: var(--fui-red); }
.err-text { color: var(--fui-red); font-size: 13px; }
</style>
