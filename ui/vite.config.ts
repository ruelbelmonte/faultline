import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The build lands in the Go package that embeds it.
export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: '../internal/ui/dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:7700',
      '/healthz': 'http://127.0.0.1:7700',
    },
  },
})
