<script setup>
// 一套 16×16 的线性图标，比 emoji 稳、比图标库轻。
const paths = {
  grid: 'M2 2h5v5H2zM9 2h5v5H9zM2 9h5v5H2zM9 9h5v5H9z',
  chat: 'M2.5 3h11v8h-6l-3 3v-3h-2z',
  users: 'M6 8a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5zM1.5 14c0-2.2 2-4 4.5-4s4.5 1.8 4.5 4M11 9.2c1.8.4 3 1.9 3 3.8',
  cpu: 'M5 5h6v6H5zM6.5 1.5v2M9.5 1.5v2M6.5 12.5v2M9.5 12.5v2M1.5 6.5h2M1.5 9.5h2M12.5 6.5h2M12.5 9.5h2',
  wave: 'M1 8h2l1.5-4 2 8 2-10 2 7 1.5-1H15',
  plug: 'M8 1.5v4M4.5 5.5h7v3a3.5 3.5 0 0 1-7 0zM8 12.5v2',
  sliders: 'M2 5h8M12 5h2M2 11h3M7 11h7M10 3v4M5 9v4',
  gear: 'M8 10.2a2.2 2.2 0 1 0 0-4.4 2.2 2.2 0 0 0 0 4.4zM8 1.5l1 1.6 1.9-.3.5 1.8 1.6 1-.8 1.8.8 1.8-1.6 1-.5 1.8-1.9-.3-1 1.6-1-1.6-1.9.3-.5-1.8-1.6-1 .8-1.8-.8-1.8 1.6-1 .5-1.8 1.9.3z',
  power: 'M8 1.5v6M4.2 4a5 5 0 1 0 7.6 0',
  download: 'M8 2v8M4.5 7L8 10.5 11.5 7M2.5 13.5h11',
  refresh: 'M13 8a5 5 0 1 1-1.6-3.7M13 2v3.5h-3.5',
  play: 'M4 3l9 5-9 5z',
  plus: 'M8 3v10M3 8h10',
  monitor: 'M2 3.5h12v8H2zM5.5 14h5M8 11.5V14',
  moon: 'M13 9.6A5.6 5.6 0 0 1 6.4 3a5.6 5.6 0 1 0 6.6 6.6z',
  chevronLeft: 'M10 3.5L5.5 8l4.5 4.5',
  chevronRight: 'M6 3.5L10.5 8 6 12.5',
  info: 'M8 7v5M8 4.2v.6M8 1.5a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13z',
  trash: 'M3 4.5h10M6 4.5V3h4v1.5M5 4.5l.6 9h4.8l.6-9',
  check: 'M3 8.5l3.5 3.5L13 5',
  close: 'M4 4l8 8M12 4l-8 8',
  copy: 'M5.5 5.5h7v8h-7zM3 3v8',
  clock: 'M8 4.5V8l2.5 1.5M8 1.5a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13z',
  search: 'M7 11.5a4.5 4.5 0 1 0 0-9 4.5 4.5 0 0 0 0 9zM10.5 10.5l4 4',
  sun: 'M8 10.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5zM8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M12.6 3.4l-1.4 1.4M4.8 11.2l-1.4 1.4',
}
defineProps({ name: { type: String, required: true }, size: { type: Number, default: 16 } })
</script>

<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 16 16"
    fill="none"
    stroke="currentColor"
    stroke-width="1.3"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    style="flex: none"
  >
    <path :d="paths[name] || paths.grid" />
  </svg>
</template>
