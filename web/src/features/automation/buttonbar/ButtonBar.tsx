/*
 * Button bar (AUTO-4): named rows of quick buttons shown in the status bar for the active terminal. A bar can be
 * limited to protocols / connection tags / connections. Buttons send text (C escapes), run a snippet, play a macro,
 * run a script or run any command. Toggle with automation.buttonBar.toggle; edit in the button bar editor.
 */
import * as React from 'react'
import { MoreHorizontal, Settings2 } from 'lucide-react'
import { toast } from 'sonner'
import { useConnection } from '@/api/connections'
import { queryClient } from '@/api/queryClient'
import { useRuntimeSession } from '@/api/sessions'
import type { Connection, RuntimeSession, Snippet } from '@/api/types'
import type { Macro } from '../types'
import { runCommand } from '@/app/commands'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Tooltip } from '@/components/ui/tooltip'
import { getActiveTerminal, useActiveTerminalInfo } from '@/features/terminal/bus'
import { errorMessage } from '@/lib/utils'
import { autoKeys, listMacros, listSnippets, runScript } from '../api'
import { unescapeText } from '../escapes'
import { playMacro } from '../macros/play'
import { resolveTargets, sendRaw, sendSnippet } from '../send'
import { automationSettings } from '../settings'
import { openButtonEditor } from '../store'
import type { ButtonBar, QuickButton } from '../types'
import { cssColor } from '../highlight/rules'
import { openAutomationTab } from '../tab/open'

function barApplies(bar: ButtonBar, session: RuntimeSession | undefined, conn: Connection | undefined): boolean {
  if (bar.protocols?.length && !(session && bar.protocols.includes(session.protocol))) return false
  if (bar.connectionIds?.length && !(session?.connectionId && bar.connectionIds.includes(session.connectionId))) return false
  if (bar.tags?.length) {
    const tags = new Set((conn?.tags ?? []).map((t) => t.toLowerCase()))
    if (!bar.tags.some((t) => tags.has(t.toLowerCase()))) return false
  }
  return true
}

async function cached<T>(key: readonly unknown[], fetcher: () => Promise<T>): Promise<T> {
  return queryClient.fetchQuery({ queryKey: key, queryFn: fetcher, staleTime: 30_000 })
}

/** Run a quick button. */
async function runButton(b: QuickButton): Promise<void> {
  const target = b.target ?? 'active'
  try {
    switch (b.action) {
      case 'send': {
        const data = unescapeText(b.text ?? '')
        if (!data) return
        await sendRaw(data, resolveTargets(target))
        getActiveTerminal()?.focus()
        return
      }
      case 'snippet': {
        const s = (await cached<Snippet[]>(autoKeys.snippets, listSnippets)).find((x) => x.id === b.refId)
        if (!s) throw new Error('The snippet no longer exists')
        await sendSnippet(s, resolveTargets(target))
        return
      }
      case 'macro': {
        const m = (await cached<Macro[]>(autoKeys.macros, listMacros)).find((x) => x.id === b.refId)
        if (!m) throw new Error('The macro no longer exists')
        await playMacro(m, target)
        return
      }
      case 'script': {
        if (!b.refId) return
        const t = getActiveTerminal()
        const r = await runScript(b.refId, { sessionId: t?.sessionId })
        toast.success(`Script started${t ? ` in ${t.info().title}` : ''}`, {
          action: { label: 'Show log', onClick: () => openAutomationTab({ page: 'scripts', scriptId: b.refId, runId: r.runId }) },
        })
        return
      }
      case 'command': {
        if (!b.text) return
        let args: unknown
        if (b.args?.trim()) {
          try {
            args = JSON.parse(b.args)
          } catch {
            args = b.args
          }
        }
        if (!(await runCommand(b.text, args, { source: 'api' }))) toast.warning(`“${b.label}”: the command is not available`)
        return
      }
    }
  } catch (err) {
    toast.error(`“${b.label}” failed`, { description: errorMessage(err) })
  }
}

const MAX_INLINE = 8

export function ButtonBarStatusItem() {
  const visible = automationSettings.useValue('buttonBarVisible')
  const bars = automationSettings.useValue('buttonBars')
  const info = useActiveTerminalInfo()
  const session = useRuntimeSession(info?.sessionId)
  const { data: conn } = useConnection(session?.connectionId)
  const buttons = React.useMemo(() => {
    if (!info) return []
    return bars.filter((b) => barApplies(b, session, conn)).flatMap((b) => b.buttons)
  }, [bars, info, session, conn])
  if (!visible || !info || !buttons.length) return null
  const inline = buttons.slice(0, MAX_INLINE)
  const rest = buttons.slice(MAX_INLINE)
  return (
    <div className="flex h-full items-center gap-0.5 pl-1" role="toolbar" aria-label="Quick buttons">
      {inline.map((b) => (
        <Tooltip key={b.id} content={describe(b)} side="top">
          <button
            type="button"
            onClick={() => void runButton(b)}
            className="flex h-[18px] max-w-32 items-center gap-1 rounded-sm border border-border/70 px-1.5 text-2xs font-medium whitespace-nowrap outline-none hover:bg-foreground/8 focus-visible:ring-1 focus-visible:ring-ring"
          >
            {b.color && <span className="size-2 shrink-0 rounded-full" style={{ background: cssColor(b.color) }} aria-hidden />}
            <span className="truncate">{b.label}</span>
          </button>
        </Tooltip>
      ))}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button type="button" aria-label={rest.length ? `${rest.length} more buttons` : 'Button bar options'} className="flex h-[18px] items-center rounded-sm px-1 text-muted-foreground outline-none hover:bg-foreground/8 hover:text-foreground focus-visible:ring-1 focus-visible:ring-ring">
            <MoreHorizontal className="size-3.5" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent side="top" align="start">
          {rest.map((b) => (
            <DropdownMenuItem key={b.id} onSelect={() => void runButton(b)}>
              {b.color && <span className="size-2 rounded-full" style={{ background: cssColor(b.color) }} aria-hidden />}
              {b.label}
            </DropdownMenuItem>
          ))}
          {rest.length > 0 && <DropdownMenuSeparator />}
          <DropdownMenuItem onSelect={() => openButtonEditor()}>
            <Settings2 /> Edit buttons…
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => automationSettings.set({ buttonBarVisible: false })}>Hide the button bar</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}

function describe(b: QuickButton): string {
  const where = b.target === 'multiexec' ? ' (broadcast group)' : b.target === 'all' ? ' (every terminal)' : ''
  switch (b.action) {
    case 'send':
      return `Send ${JSON.stringify(b.text ?? '').slice(1, -1)}${where}`
    case 'snippet':
      return `Run snippet${where}`
    case 'macro':
      return `Play macro${where}`
    case 'script':
      return 'Run script in the active terminal'
    case 'command':
      return `Command ${b.text}`
  }
  return b.label
}
