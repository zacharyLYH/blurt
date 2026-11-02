import { fileURLToPath, URL } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Dev proxies through nginx (docker compose publishes 127.0.0.1:9996), so
// the API contract is identical to production.
const serverUrl = process.env.BLURT_SERVER_URL ?? 'http://localhost:9996'

export default defineConfig({
  // React 19's compiler memoises components and hooks at build time, so the app
  // carries no manual useMemo/useCallback for re-render cost.
  plugins: [react({ babel: { plugins: [['babel-plugin-react-compiler', {}]] } }), tailwindcss()],
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
