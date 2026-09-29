/*
 * `sessions.quickConnect` (UI-1): saved-session names (`name` or `@name`) open the saved session; anything else is
 * parsed (quickparse.ts) and opened as an unsaved session through the terminal feature, with a "Save as session…"
 * toast action. History (settings.sessions.quickConnectHistory) never contains inline passwords.
 */
import { toast } from 'sonner'
import { runCommand } from '@/app/commands'
import { setQuickConnectSanitizer } from '@/app/quickconnect'
import { defaultPort } from '@/app/protocols'
import { listConnections } from '@/api/connections'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection } from '@/api/types'
import { isPlainObject } from '@/lib/utils'
import { connectLocalShell, connectQuick, connectTo } from './connect'
import { openSessionEditor } from './dialogs/store'
import { autoName, connectionTarget } from './model'
import { QUICK_CONNECT_HELP, QuickConnectError, parseHostPort, parseQuickConnect } from './quickparse'
import { rememberQuickConnect } from './settings'

/** Saved connections (fetched when not cached yet; empty on failure so parsing still works). */
async function savedConnections(): Promise<Connection[]> {
  const cached = queryClient.getQueryData<Connection[]>(queryKeys.connections)
  if (cached) return cached
  try {
    return await queryClient.fetchQuery({ queryKey: queryKeys.connections, queryFn: listConnections })
  } catch {
    return []
  }
}

/** -J / ProxyJump targets must be saved SSH sessions: match by name, else by [user@]host[:port]. */
export function resolveJumpHost(spec: string, conns: readonly Connection[]): string | undefined {
  const ssh = conns.filter((c) => c.protocol === 'ssh')
  const s = spec.trim()
  const byName = ssh.find((c) => c.name.toLowerCase() === s.toLowerCase())
  if (byName) return byName.id
  try {
    const at = s.lastIndexOf('@')
    const user = at >= 0 ? s.slice(0, at) : ''
    const { host, port } = parseHostPort(at >= 0 ? s.slice(at + 1) : s)
    return ssh.find(
      (c) => c.host.toLowerCase() === host.toLowerCase() && (!port || (c.port || 22) === port) && (!user || c.username === user),
    )?.id
  } catch {
    return undefined
  }
}

// The ribbon field keeps its own local history of what was typed: it stores only the form this parser deems safe
// (inline passwords stripped), never the raw text.
setQuickConnectSanitizer((text) => {
  const input = text.trim()
  if (!input || input.startsWith('@')) return input || null
  try {
    const parsed = parseQuickConnect(input, {})
    return parsed.kind === 'help' ? null : parsed.sanitized
  } catch {
    return null // not a usable spec: nothing worth remembering
  }
})

function showHelp(): void {
  toast.info('Quick connect', {
    duration: 20_000,
    description: (
      <ul className="mt-1 grid gap-0.5">
        {QUICK_CONNECT_HELP.map((h) => (
          <li key={h.example} className="grid">
            <code className="font-mono text-xs">{h.example}</code>
            <span className="text-xs opacity-75">{h.description}</span>
          </li>
        ))}
      </ul>
    ),
  })
}

/** Text from command args: the raw string (ribbon) or {text}. */
export function quickConnectText(args: unknown): string {
  if (typeof args === 'string') return args
  if (isPlainObject(args) && typeof args.text === 'string') return args.text
  return ''
}

/** Run quick connect. Throws (with a user-facing message) when the text cannot be used. */
export async function runQuickConnect(args: unknown): Promise<void> {
  const input = quickConnectText(args).trim()
  if (!input) {
    await runCommand('quickConnect.focus', undefined, { source: 'api' })
    return
  }
  const conns = await savedConnections()

  // Saved session by name ("web-1" or "@web-1").
  const name = input.startsWith('@') ? input.slice(1).trim() : input
  const saved = conns.find((c) => c.name.toLowerCase() === name.toLowerCase())
  if (saved) {
    if (await connectTo(saved)) rememberQuickConnect(input)
    return
  }
  if (input.startsWith('@')) throw new QuickConnectError(`No saved session is named "${name}"`)

  const parsed = parseQuickConnect(input, { resolveJump: (spec) => resolveJumpHost(spec, conns) })
  if (parsed.kind === 'help') {
    showHelp()
    return
  }
  if (parsed.kind === 'local') {
    if (await connectLocalShell(parsed.shell)) rememberQuickConnect(parsed.sanitized)
    return
  }

  const d = parsed.draft
  const port = d.port || defaultPort(d.protocol) || undefined
  const label = autoName({ protocol: d.protocol, host: d.host ?? '', port: port ?? 0, username: d.username ?? '', options: d.options ?? {} }) || input
  const opened = await connectQuick({ ...d, port, name: label })
  if (!opened) return // already reported by the terminal feature
  rememberQuickConnect(parsed.sanitized)
  for (const w of parsed.warnings) toast.warning(w)
  const target = connectionTarget({ protocol: d.protocol, host: d.host ?? '', port: port ?? 0, username: d.username ?? '', options: d.options ?? {} })
  toast(`Connecting to ${target || label}`, {
    description: 'Unsaved session',
    action: {
      label: 'Save as session…',
      onClick: () => openSessionEditor({ mode: 'create', initial: { ...d, port, name: label } }),
    },
  })
}
