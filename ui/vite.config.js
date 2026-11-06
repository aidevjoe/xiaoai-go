import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 产物直接落到服务端的 embed 目录，go build 时打进二进制
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: '../internal/server/web/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:28080',
    },
  },
})
