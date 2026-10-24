import { fileURLToPath, URL } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Dev proxies through nginx (docker compose publishes 127.0.0.1:9998), so
// the API contract is identical to production.
const serverUrl = process.env.BLURT_SERVER_URL ?? 'http://localhost:9998'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    proxy: {
      '/api': { target: serverUrl, changeOrigin: true },
      '/health': { target: serverUrl, changeOrigin: true },
    },
  },
})
