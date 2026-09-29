import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { writeFileSync } from 'node:fs'
import path from 'node:path'

const backend = process.env.TERMSTEAD_BACKEND ?? 'http://127.0.0.1:7822'
const outDir = path.resolve(__dirname, '../internal/webui/dist')

/** emptyOutDir wipes internal/webui/dist; keep the tracked placeholder that lets go:embed compile on fresh clones. */
function keepPlaceholder(): Plugin {
  return {
    name: 'termstead-keep-placeholder',
    apply: 'build',
    closeBundle() {
      writeFileSync(path.join(outDir, '.keep'), '')
    },
  }
}

/**
 * Web fonts without a visible swap (docs/UX.md): the bundled @fontsource fonts use `font-display: block` instead of
 * `swap` (the files are local and preloaded, so the block period is a few ms and text never reflows), and the latin
 * files of the UI and terminal fonts are preloaded from index.html.
 */
const FONT_CSS_RE = /[\\/]@fontsource-variable[\\/](inter|jetbrains-mono)[\\/][^\\/]*\.css$/
const PRELOAD_FONTS = [/inter-latin-wght-normal/, /jetbrains-mono-latin-wght-normal/]

function calmFonts(): Plugin {
  return {
    name: 'termstead-calm-fonts',
    enforce: 'pre',
    transform(code, id) {
      if (!FONT_CSS_RE.test(id.split('?')[0])) return null
      return { code: code.replaceAll('font-display: swap', 'font-display: block'), map: null }
    },
    transformIndexHtml: {
      order: 'post',
      handler(_html, ctx) {
        if (!ctx.bundle) return []
        const files = Object.values(ctx.bundle)
          .filter((f) => f.type === 'asset' && /\.woff2$/.test(f.fileName))
          .map((f) => f.fileName)
        return PRELOAD_FONTS.flatMap((re) => files.filter((f) => re.test(f)).slice(0, 1)).map((f) => ({
          tag: 'link',
          attrs: { rel: 'preload', as: 'font', type: 'font/woff2', href: `/${f}`, crossorigin: '' },
          injectTo: 'head-prepend' as const,
        }))
      },
    },
  }
}

export default defineConfig({
  plugins: [calmFonts(), react(), tailwindcss(), keepPlaceholder()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
      // monaco-vim (and other Monaco add-ons) import "monaco-editor/esm/vs/…", but since monaco-editor 0.56 the package
      // "exports" map resolves "monaco-editor/<path>" to "esm/vs/<path>.js", so those specifiers point at files that do
      // not exist. Map the old deep-import prefix onto the real folder (same files, so one Monaco instance).
      'monaco-editor/esm/vs': path.resolve(__dirname, 'node_modules/monaco-editor/esm/vs'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: backend, changeOrigin: true },
      '/ws': { target: backend, ws: true, changeOrigin: true },
    },
  },
  build: {
    outDir,
    emptyOutDir: true,
    chunkSizeWarningLimit: 4096,
    sourcemap: false,
    // ES2022 keeps top-level await working (noVNC) and matches every browser Termstead supports.
    target: 'es2022',
    rolldownOptions: {
      output: {
        codeSplitting: {
          // Without these groups every icon / Radix primitive shared by lazy chunks becomes its own tiny file.
          groups: [
            { name: 'icons', test: /[\\/]node_modules[\\/]lucide-react[\\/]/ },
            { name: 'radix', test: /[\\/]node_modules[\\/](@radix-ui|radix-ui)[\\/]/ },
          ],
        },
      },
    },
  },
})
