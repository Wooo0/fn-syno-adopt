<script setup lang="ts">
// 按钮：对齐 fnOS 第三方应用标杆观感
// primary=蓝实心 / default=白底描边 / ghost=浅灰信息条内按钮 / danger=浅红底红字
withDefaults(defineProps<{
  variant?: 'primary' | 'default' | 'ghost' | 'danger'
  disabled?: boolean
}>(), { variant: 'primary', disabled: false })
</script>

<template>
  <button class="ui-btn" :class="`ui-btn--${variant}`" :disabled="disabled">
    <slot />
  </button>
</template>

<style scoped>
.ui-btn {
  border: 0;
  padding: 5px 16px;
  border-radius: var(--fui-radius-sm);
  font-size: 13px;
  font-family: inherit;
  cursor: pointer;
  transition: background-color 0.15s ease, border-color 0.15s ease;
}
.ui-btn--primary { background: var(--fui-primary); color: #FFF; }
.ui-btn--primary:hover:not(:disabled) { background: var(--fui-primary-hover); }

.ui-btn--default {
  background: var(--fui-card);
  color: var(--fui-text-1);
  border: 1px solid var(--fui-border);
}
.ui-btn--default:hover:not(:disabled) { border-color: var(--fui-primary); color: var(--fui-primary); }

.ui-btn--ghost {
  background: var(--fui-panel);
  color: var(--fui-text-1);
  border: 1px solid transparent;
}
.ui-btn--ghost:hover:not(:disabled) { color: var(--fui-primary); }

.ui-btn--danger {
  background: var(--fui-danger-bg);
  color: var(--fui-red);
}
.ui-btn--danger:hover:not(:disabled) { background: rgba(220, 38, 38, 0.15); }

.ui-btn:disabled { opacity: 0.5; cursor: default; }
</style>
