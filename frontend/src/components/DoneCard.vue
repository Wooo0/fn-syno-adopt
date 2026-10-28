<script setup lang="ts">
// 完成卡片：把"这一步做完了，接下来该干什么"显式摆出来。
// 只用于终态为 done 的任务，且由调用方用 job.kind 精确匹配（绝不误触发）。
defineProps<{
  title: string
  items: string[]
  tone?: 'ok' | 'info'
}>()
</script>

<template>
  <div class="done-card" :class="`is-${tone ?? 'ok'}`">
    <div class="done-head">
      <span class="done-mark">{{ (tone ?? 'ok') === 'ok' ? '✓' : 'ⓘ' }}</span>
      <b>{{ title }}</b>
    </div>
    <ol class="done-list">
      <li v-for="(it, i) in items" :key="i">{{ it }}</li>
    </ol>
    <div v-if="$slots.default" class="done-actions"><slot /></div>
  </div>
</template>

<style scoped>
.done-card {
  margin-top: 14px;
  border-radius: 8px;
  padding: 12px 16px;
  font-size: 13px;
}
.done-card.is-ok {
  background: rgba(0, 167, 87, 0.07);
  border: 1px solid rgba(0, 167, 87, 0.25);
}
.done-card.is-info {
  background: rgba(55, 140, 190, 0.07);
  border: 1px solid rgba(55, 140, 190, 0.25);
}
.done-head { display: flex; align-items: center; gap: 8px; }
.done-head .done-mark { color: var(--fui-green); font-weight: 700; }
.is-info .done-head .done-mark { color: var(--fui-blue); }
.done-list {
  margin: 8px 0 0;
  padding-left: 20px;
  font-size: 12px;
  color: var(--fui-text-2);
}
.done-list li { margin: 3px 0; }
.done-actions { margin-top: 10px; display: flex; gap: 8px; flex-wrap: wrap; }
</style>
