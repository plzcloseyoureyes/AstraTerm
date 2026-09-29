/*
 * Node test harness for the files feature's pure modules (no DOM, no React): lets `node --test` import the TypeScript
 * sources the way Vite does (extensionless relative imports, the "@/" alias). Node strips the types itself.
 *
 *   cd web && node --import ./src/features/files/__tests__/register.mjs --test 'src/features/files/__tests__/*.test.mjs'
 */
import * as nodeModule from 'node:module'
import { resolve } from './hooks.mjs'

if (typeof nodeModule.registerHooks === 'function') {
  // In-thread, synchronous hooks (Node ≥ 22.15 / 23.5).
  nodeModule.registerHooks({ resolve })
} else {
  nodeModule.register('./hooks.mjs', import.meta.url)
}
