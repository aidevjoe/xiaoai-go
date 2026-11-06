<script setup>
// 品牌标识：五根音量柱（中间一根橙色）。
//
// 内联成组件而不是引图片：能跟随容器尺寸缩放，也不用为 2x/3x 出多套位图。
// 图形与仓库根目录的 assets/logo.svg 保持一致，改的时候两边一起改。
defineProps({
  size: { type: [Number, String], default: 30 },
})
</script>

<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 512 512"
    role="img"
    aria-label="小爱GO"
    style="flex: none"
  >
    <rect width="512" height="512" rx="116" fill="#101B2E" />
    <rect x="84" y="196" width="48" height="120" rx="24" fill="#94A3B8" opacity=".8" />
    <rect x="158" y="156" width="48" height="200" rx="24" fill="#E2E8F0" opacity=".95" />
    <rect x="232" y="112" width="48" height="288" rx="24" fill="#F97316" />
    <rect x="306" y="156" width="48" height="200" rx="24" fill="#E2E8F0" opacity=".95" />
    <rect x="380" y="196" width="48" height="120" rx="24" fill="#94A3B8" opacity=".8" />
  </svg>
</template>
