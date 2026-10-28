<template>
  <section class="ui-card">
    <div v-if="$slots.title || $slots.actions" class="ui-card__hd">
      <div>
        <h2 class="ui-card__title"><slot name="title" /></h2>
        <p v-if="$slots.desc" class="ui-card__desc"><slot name="desc" /></p>
      </div>
      <div class="ui-card__actions"><slot name="actions" /></div>
    </div>
    <slot />
  </section>
</template>

<style scoped>
.ui-card {
  background: var(--fui-card);
  border: 1px solid var(--fui-border);
  border-radius: var(--fui-radius);
  padding: 18px 22px;
  margin-bottom: 16px;
}
.ui-card__hd {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: 12px;
}
.ui-card__title {
  font-size: 15px;
  font-weight: 600;
  margin: 0;
  color: var(--fui-text-1);
}
.ui-card__desc {
  margin: 4px 0 0;
  font-size: 12px;
  color: var(--fui-text-2);
  max-width: 520px;
}
.ui-card__actions { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
</style>
