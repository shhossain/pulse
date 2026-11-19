import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Pages are served by the Pulse app from the embedded dist/ folder.
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: { input: { popover: 'popover.html', run: 'run.html' } },
  },
})
