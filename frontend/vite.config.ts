import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发时 npm run dev，/api 代理到本机 Go 后端
// base './'：统一网关模式下页面挂在 /app/fn-syno-adopt/ 前缀下，资源必须用相对路径
export default defineConfig({
  base: './',
  plugins: [vue()],
  server: {
    proxy: { '/api': 'http://127.0.0.1:18099' },
  },
  build: { outDir: 'dist' },
})
