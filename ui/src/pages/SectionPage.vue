<script setup>
// 带分区的页面容器。
//
// 之前这里有一排顶部 tab + 页内大标题 + 底部「上一组/下一组」，
// 和侧栏的分区目录重复了三遍；而且「容器内再套一个居中容器」
// 会让标题和下面的卡片左边缘对不齐。
// 现在只做一件事：提供统一的页面容器，内容交给分区组件。
// 切换分区靠侧栏目录（桌面）/ 顶栏下拉（手机），当前位置由顶栏标题说明。
import { computed } from 'vue'

const props = defineProps({
  module: { type: Object, required: true },
  section: { type: String, default: '' },
})

const sections = computed(() => props.module.sections || [])
const active = computed(
  () => sections.value.find((s) => s.id === props.section) || sections.value[0],
)
</script>

<template>
  <div class="page">
    <component :is="active.component" :key="active.id" />
  </div>
</template>
