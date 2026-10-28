<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { api, type Job } from '../api'

const props = defineProps<{ jobId: string | null }>()
const emit = defineEmits<{ (e: 'finished', job: Job | null): void }>()

const job = ref<Job | null>(null)
const error = ref('')
let timer: number | undefined

async function poll() {
  if (!props.jobId) return
  try {
    const res = await api.job(props.jobId)
    job.value = res.job
    error.value = ''
    if (res.job.status === 'done' || res.job.status === 'error') {
      stop()
      emit('finished', res.job)
    }
  } catch (e) {
    // 轮询失败也必须 emit：否则调用方（AdoptPage）的 running 会永久转圈
    error.value = e instanceof Error ? e.message : String(e)
    stop()
    emit('finished', { id: props.jobId ?? '', kind: '', status: 'error', steps: [], error: error.value })
  }
}

function start() {
  stop()
  job.value = null
  if (!props.jobId) return
  timer = window.setInterval(poll, 1200)
  poll()
}

function stop() {
  if (timer !== undefined) {
    window.clearInterval(timer)
    timer = undefined
  }
}

watch(() => props.jobId, start, { immediate: true })
onBeforeUnmount(stop)

const statusText: Record<string, string> = {
  ok: '成功', fail: '失败', skip: '跳过', pending: '…', warn: '警告',
}
</script>

<template>
  <div v-if="jobId" class="job-log">
    <div class="job-head">
      <span class="job-id mono">{{ jobId }}</span>
      <span v-if="job?.status === 'running'" class="job-state running">执行中…</span>
      <span v-else-if="job?.status === 'done'" class="job-state done">✓ 完成</span>
      <span v-else-if="job?.status === 'error'" class="job-state err">✗ 失败：{{ job.error }}</span>
    </div>
    <p v-if="error" class="poll-err">日志轮询失败：{{ error }}</p>
    <ol v-if="job?.steps?.length" class="steps">
      <li v-for="(s, i) in job.steps" :key="i" :class="`st-${s.status}`">
        <div class="st-line">
          <span class="st-mark">{{ { ok: '✓', fail: '✗', skip: '–', pending: '…', warn: '⚠' }[s.status] }}</span>
          <span class="st-name">{{ s.name }}</span>
          <span class="st-status">{{ statusText[s.status] }}</span>
        </div>
        <div v-if="s.command" class="st-cmd mono">{{ s.command }}</div>
        <div v-if="s.detail" class="st-detail">{{ s.detail }}</div>
      </li>
    </ol>
  </div>
</template>

<style scoped>
.job-log { margin-top: 4px; }
.job-head { display: flex; align-items: center; gap: 10px; margin-bottom: 8px; }
.job-id { color: var(--fui-text-2); font-size: 12px; }
.job-state { font-size: 13px; font-weight: 600; }
.job-state.running { color: var(--fui-blue); }
.job-state.done { color: var(--fui-green); }
.job-state.err { color: var(--fui-red); }
.poll-err { color: var(--fui-red); font-size: 12px; margin: 0 0 8px; }

.steps { list-style: none; margin: 0; padding: 0; }
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
</style>
