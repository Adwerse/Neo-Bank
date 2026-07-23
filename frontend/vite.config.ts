import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // The Gateway (services/gateway) exposes routes directly under
      // /auth/*, /accounts/*, etc — it has no "/api" prefix of its own (see
      // gateway/proxy.go). The frontend calls everything under "/api/*" so
      // the browser only ever sees one origin (this dev server); the rewrite
      // strips "/api" before forwarding to the Gateway. This removes the
      // CORS problem entirely in dev — production needs a different answer
      // (Gateway serving the built static files, or CORS headers), see the
      // root README.
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, ''),
      },
    },
  },
})
