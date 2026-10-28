<template>
  <div class="ui-empty">
    <p class="ui-empty__main"><slot /></p>
    <p v-if="$slots.hint" class="ui-empty__hint"><slot name="hint" /></p>
  </div>
</template>

<style scoped>
.ui-empty { text-align: center; padding: 28px 0 20px; }
.ui-empty__main { margin: 4px 0; color: var(--fui-text-1); }
.ui-empty__hint { margin: 4px 0; color: var(--fui-text-2); font-size: 12px; }
</style>
