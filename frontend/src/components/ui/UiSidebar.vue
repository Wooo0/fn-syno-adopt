<script setup lang="ts">
// 侧栏导航：对齐 fnOS 第三方应用标杆观感
// （选中项白底圆角卡片 + 蓝字，底部版本号 + 运行状态徽章）
export interface SidebarItem {
  key: string
  label: string
  disabled?: boolean
}

defineProps<{
  items: SidebarItem[]
  modelValue: string
  version?: string
  statusOk?: boolean
}>()

defineEmits<{ (e: 'update:modelValue', key: string): void }>()
</script>

<template>
  <aside class="ui-side">
    <nav class="ui-side__nav">
      <button
        v-for="it in items"
        :key="it.key"
        class="ui-side__item"
        :class="{ 'ui-side__item--active': it.key === modelValue, 'ui-side__item--disabled': it.disabled }"
        :disabled="it.disabled"
        @click="!it.disabled && $emit('update:modelValue', it.key)"
      >
        <span class="ui-side__icon"><slot name="icon" :item="it" /></span>
        <span>{{ it.label }}</span>
      </button>
    </nav>

    <div class="ui-side__footer">
      <!-- 迷你流程条：检测 → 接管 → 清理，当前节点高亮（可选插槽） -->
      <div v-if="$slots.flow" class="ui-side__flow"><slot name="flow" /></div>
      <!-- 运行中任务指示（可选插槽）：切页后仍能看见进度并跳回 -->
      <div v-if="$slots.task"><slot name="task" /></div>
      <div v-if="version" class="ui-side__ver">{{ version }}</div>
      <span v-if="statusOk !== undefined" class="ui-side__status" :class="statusOk ? 'is-ok' : 'is-err'">
        {{ statusOk ? '运行正常' : '服务离线' }}
      </span>
    </div>
  </aside>
</template>

<style scoped>
.ui-side {
  width: 168px;
  flex-shrink: 0;
  background: transparent;
  display: flex;
  flex-direction: column;
  padding: 12px 10px;
}
.ui-side__nav { display: flex; flex-direction: column; gap: 2px; overflow-y: auto; }
.ui-side__item {
  display: flex;
  align-items: center;
  gap: 8px;
  border: 0;
  background: transparent;
  color: var(--fui-text-1);
  font-size: 13px;
  font-family: inherit;
  text-align: left;
  padding: 9px 12px;
  border-radius: 8px;
  cursor: pointer;
  transition: background-color 0.15s ease;
}
.ui-side__item:hover:not(:disabled):not(.ui-side__item--active) { background: rgba(255, 255, 255, 0.7); }
.ui-side__item--active {
  background: var(--fui-card);
  color: var(--fui-primary);
  font-weight: 600;
  box-shadow: 0 1px 2px rgba(58, 68, 82, 0.06);
}
.ui-side__item--disabled { color: var(--fui-text-disabled); cursor: default; }
.ui-side__icon { display: inline-flex; width: 16px; height: 16px; }
.ui-side__icon :deep(svg) { width: 16px; height: 16px; }

.ui-side__footer {
  margin-top: auto;
  padding: 10px 12px 4px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.ui-side__ver { font-size: 12px; color: var(--fui-text-2); }
.ui-side__status {
  align-self: flex-start;
  font-size: 12px;
  padding: 1px 10px;
  border-radius: 999px;
}
.ui-side__status.is-ok { background: rgba(0, 167, 87, 0.1); color: var(--fui-green); }
.ui-side__status.is-err { background: var(--fui-danger-bg); color: var(--fui-red); }
</style>
