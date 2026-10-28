<script setup lang="ts">
// 徽章：轻量样式（10% 透明度底 + 语义色文字）
withDefaults(defineProps<{
  type?: 'ok' | 'warn' | 'err' | 'info'
}>(), { type: 'info' })
</script>

<template>
  <span class="ui-badge" :class="`ui-badge--${type}`"><slot /></span>
</template>

<style scoped>
.ui-badge {
  display: inline-block;
  padding: 1px 10px;
  border-radius: 4px;
  font-size: 12px;
  white-space: nowrap;
}
.ui-badge--ok { background: rgba(0, 167, 87, 0.1); color: var(--fui-green); }
.ui-badge--warn { background: rgba(245, 157, 0, 0.12); color: var(--fui-yellow); }
.ui-badge--err { background: rgba(223, 74, 50, 0.08); color: var(--fui-red); }
.ui-badge--info { background: rgba(0, 114, 165, 0.1); color: var(--fui-primary); }
</style>
