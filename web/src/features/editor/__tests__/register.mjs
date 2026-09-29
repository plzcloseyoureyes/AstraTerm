/*
 * Node test harness for the editor's pure modules (no DOM): lets `node --test` import the TypeScript sources the way
 * Vite does (extensionless relative imports, the "@/" alias). Node strips the types itself.
 *
 *   cd web && node --import ./src/features/editor/__tests__/register.mjs --test src/features/editor/__tests__/
 */
import { register } from 'node:module'

register('./hooks.mjs', import.meta.url)
