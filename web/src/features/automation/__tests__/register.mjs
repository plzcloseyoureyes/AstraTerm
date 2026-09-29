/*
 * Node test harness for the automation feature (pure modules; no DOM): lets `node --test` import the TypeScript sources the way
 * Vite does (extensionless relative imports, the "@/" alias). Node strips the types itself.
 *
 *   cd web && node --import ./src/features/automation/__tests__/register.mjs --test 'src/features/automation/__tests__/*.test.mjs'
 */
import * as nodeModule from 'node:module'
import { resolve } from './hooks.mjs'

if (typeof nodeModule.registerHooks === 'function') {
  nodeModule.registerHooks({ resolve })
} else {
  nodeModule.register('./hooks.mjs', import.meta.url)
}
