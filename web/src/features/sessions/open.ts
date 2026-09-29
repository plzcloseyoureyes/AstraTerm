/*
 * SPEC §7 names src/features/sessions/open.ts as the place to open connections; the implementation lives in the
 * terminal feature (src/features/terminal/open.ts, owned by F1a). Re-exported here so both import paths work.
 */
export { attachSession, openConnection, openLocalShell, openQuick } from '@/features/terminal/open'
export { connectMany, connectQuick, connectSafely, connectTo, jumpToSession } from './connect'
