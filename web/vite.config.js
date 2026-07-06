import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The app is served by the daemon under the /ui/ prefix (same origin as the
// API), so `base` must be /ui/ or every emitted asset URL would 404. The build
// output goes straight into the Go embed directory; it is committed so that
// `go build` works without a Node toolchain (CI + e2e are node-free).
export default defineConfig({
  plugins: [svelte()],
  base: '/ui/',
  build: {
    outDir: '../cmd/goose-daemon/uidist',
    emptyOutDir: true,
  },
})
