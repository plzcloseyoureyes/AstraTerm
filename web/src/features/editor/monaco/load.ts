/*
 * Lazy access to the Monaco bundle (monaco/setup.ts). Tabs start loading it as soon as they mount — in parallel with
 * reading the file — and create their editor once it is there. The editor font is loaded first so Monaco measures
 * the right glyph widths.
 */
export type MonacoModule = typeof import('./setup')

let promise: Promise<MonacoModule> | null = null
let loaded: MonacoModule | null = null

function editorFontFamily(): string {
  try {
    return getComputedStyle(document.documentElement).getPropertyValue('--font-mono').trim()
  } catch {
    return ''
  }
}

/** The bundled monospace font (JetBrains Mono Variable) — waited for at most `ms`. */
async function fontReady(ms: number): Promise<void> {
  if (typeof document === 'undefined' || !('fonts' in document)) return
  const first = editorFontFamily().split(',')[0]?.trim() || '"JetBrains Mono Variable"'
  try {
    await Promise.race([
      Promise.all([document.fonts.load(`400 13px ${first}`), document.fonts.load(`700 13px ${first}`)]),
      new Promise((resolve) => setTimeout(resolve, ms)),
    ])
  } catch {
    /* measured later (setup remeasures when fonts finish loading) */
  }
}

/** Load (once) and return the Monaco module. A failed load (network, stale deployment) can be retried. */
export function loadMonaco(): Promise<MonacoModule> {
  if (loaded) return Promise.resolve(loaded)
  if (!promise) {
    const p = Promise.all([import('./setup'), fontReady(1500)]).then(([m]) => {
      loaded = m
      return m
    })
    p.catch(() => {
      if (promise === p) promise = null
    })
    promise = p
  }
  return promise
}

/** The Monaco module when it has been loaded already (synchronous paths: export, printing). */
export function loadedMonaco(): MonacoModule | null {
  return loaded
}
