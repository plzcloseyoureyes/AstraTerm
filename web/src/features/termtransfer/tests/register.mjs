/*
 * Node test loader for the term-transfer engine modules (TypeScript run through Node's type stripping):
 * extensionless relative imports and the `@/` alias resolve to .ts files, and `trzsz` resolves to its ESM build (the
 * one browsers get through Vite).
 *
 *   cd web && node --import ./src/features/termtransfer/tests/register.mjs --test src/features/termtransfer/tests/
 */
import fs from 'node:fs'
import path from 'node:path'
import { registerHooks } from 'node:module'
import { fileURLToPath, pathToFileURL } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const webRoot = path.resolve(here, '../../../..')
const srcRoot = path.join(webRoot, 'src')

function tryFile(base) {
  for (const cand of [base, `${base}.ts`, `${base}.tsx`, path.join(base, 'index.ts')]) {
    try {
      if (fs.statSync(cand).isFile()) return cand
    } catch {
      /* next */
    }
  }
  return null
}

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier === 'trzsz') {
      return { url: pathToFileURL(path.join(webRoot, 'node_modules/trzsz/lib/trzsz.mjs')).href, shortCircuit: true }
    }
    let base = null
    if (specifier.startsWith('@/')) base = path.join(srcRoot, specifier.slice(2))
    else if ((specifier.startsWith('./') || specifier.startsWith('../')) && context.parentURL?.startsWith('file:')) {
      base = path.resolve(path.dirname(fileURLToPath(context.parentURL)), specifier)
    }
    if (base && !path.extname(base)) {
      const file = tryFile(base)
      if (file) return { url: pathToFileURL(file).href, shortCircuit: true }
    }
    return nextResolve(specifier, context)
  },
})
