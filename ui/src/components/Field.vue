<script setup>
// 表单字段的外壳：标签 + 说明 + 可选的状态标记。
defineProps({
  label: String,
  hint: String,
  hintKind: { type: String, default: '' }, // '' | 'warn'
  tag: String,
  tagKind: { type: String, default: '' },
  wide: Boolean,
})
</script>

<template>
  <div class="field">
    <div v-if="label || $slots.labelExtra" class="field-label">
      <span v-if="label">{{ label }}</span>
      <span v-if="tag" class="tag" :class="tagKind">{{ tag }}</span>
      <slot name="labelExtra" />
    </div>
    <slot />
    <div v-if="hint" class="field-hint" :class="hintKind">{{ hint }}</div>
  </div>
</template>
