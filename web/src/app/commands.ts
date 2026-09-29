/*
 * Command helpers on top of the command registry.
 *
 *   await runCommand('sessions.connect', {id})        → true if it ran (false: unknown / unavailable / threw)
 *   const cmd = useCommand('settings.open'); cmd.enabled; cmd.run()
 *   getKeybindings('palette.open')                    → effective bindings (user overrides ← defaults)
 */
import { useCallback, useMemo } from 'react'
import { toast } from 'sonner'
import { activeTab } from '@/stores/workspace'
import { keybindingSettings } from '@/stores/settings'
import { errorMessage } from '@/lib/utils'
import { commands, type CommandDef, type CommandSource } from './registry'

export interface RunOptions {
  source?: CommandSource
  event?: KeyboardEvent
}

function isAvailable(cmd: CommandDef): boolean {
  if (!cmd.when) return true
  try {
    return !!cmd.when()
  } catch (err) {
    console.error(`[commands] when() of "${cmd.id}" failed`, err)
    return false
  }
}

/** Run a command by id. Errors are reported with a toast; resolves whether the command ran successfully. */
export async function runCommand(id: string, args?: unknown, opts: RunOptions = {}): Promise<boolean> {
  const cmd = commands.get(id)
  if (!cmd) {
    if (import.meta.env.DEV) console.warn(`[commands] "${id}" is not registered`)
    return false
  }
  if (!isAvailable(cmd)) return false
  try {
    await cmd.run({ args, source: opts.source ?? 'api', activeTab: activeTab(), event: opts.event })
    return true
  } catch (err) {
    console.error(`[commands] "${id}" failed`, err)
    toast.error(`${cmd.title} failed`, { description: errorMessage(err) })
    return false
  }
}

/** Registered and currently available? */
export function isCommandEnabled(id: string): boolean {
  const cmd = commands.get(id)
  return !!cmd && isAvailable(cmd)
}

export function toBindingList(b: string | string[] | undefined): string[] {
  if (!b) return []
  return (Array.isArray(b) ? b : [b]).filter((x) => typeof x === 'string' && x.trim() !== '')
}

/** Effective keybindings of a command (user override wins; an empty override unbinds). */
export function getKeybindings(id: string, overrides = keybindingSettings.get()): string[] {
  const o = overrides[id]
  if (Array.isArray(o)) return toBindingList(o)
  return toBindingList(commands.get(id)?.keybinding)
}

/** React: effective keybindings of a command. */
export function useKeybindings(id: string | undefined): string[] {
  const overrides = keybindingSettings.use()
  const cmd = commands.useItem(id)
  return useMemo(() => {
    if (!id) return []
    const o = overrides[id]
    if (Array.isArray(o)) return toBindingList(o)
    return toBindingList(cmd?.keybinding)
  }, [id, overrides, cmd])
}

/** React: a command handle. `enabled` re-evaluates `when()` on every render of the caller. */
export function useCommand(id: string | undefined) {
  const command = commands.useItem(id)
  const keybindings = useKeybindings(id)
  const enabled = !!command && isAvailable(command)
  const run = useCallback((args?: unknown, source: CommandSource = 'api') => (id ? runCommand(id, args, { source }) : Promise.resolve(false)), [id])
  return { command, enabled, keybindings, run }
}
