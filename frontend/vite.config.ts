import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The dev server proxies /api to the backend so that the browser talks to one
// origin in development exactly as it does in production behind nginx. That is
// deliberate: the backend ships no CORS headers, and a frontend that only works
// because dev happens to be same-origin-by-proxy is a frontend that keeps
// working when it is packaged.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.HALIPHRON_API ?? 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
  build: { outDir: 'dist', sourcemap: true },
})
