// Module resolution hooks for register.mjs: "@/x" → web/src/x, and extensionless relative imports → .ts / .tsx.
import { existsSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..')

function isFile(p) {
  try {
    return statSync(p).isFile()
  } catch {
    return false
  }
}

export async function resolve(specifier, context, next) {
  let spec = specifier
  if (spec.startsWith('@/')) spec = pathToFileURL(path.join(SRC, spec.slice(2))).href
  const local = spec.startsWith('.') || spec.startsWith('/') || spec.startsWith('file:')
  if (local && !/\.(?:[cm]?[jt]sx?|json)$/.test(spec)) {
    const base = spec.startsWith('file:') ? fileURLToPath(spec) : spec.startsWith('/') ? spec : fileURLToPath(new URL(spec, context.parentURL))
    for (const ext of ['.ts', '.tsx', '/index.ts']) {
      if (existsSync(base + ext) && isFile(base + ext)) return next(pathToFileURL(base + ext).href, context)
    }
  }
  return next(spec, context)
}
