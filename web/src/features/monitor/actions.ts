/*
 * User-facing actions with confirmation, progress toasts and the "permission denied → retry with sudo?" flow. sudo
 * may ask for a password through the prompt broker (the shell's PromptHost shows it).
 */
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { confirm, prompt } from '@/components/ui/dialog-host'
import { errorMessage } from '@/lib/utils'
import { killProcess, monitorKeys, reniceProcess, serviceAction } from './api'
import type { ProcessInfo, ServiceAction, TargetId } from './types'

export const SIGNALS: { id: string; label: string; hint: string }[] = [
  { id: 'TERM', label: 'SIGTERM', hint: 'Ask the process to exit' },
  { id: 'KILL', label: 'SIGKILL', hint: 'Terminate immediately' },
  { id: 'INT', label: 'SIGINT', hint: 'Interrupt (Ctrl+C)' },
  { id: 'HUP', label: 'SIGHUP', hint: 'Hang up / reload configuration' },
  { id: 'QUIT', label: 'SIGQUIT', hint: 'Quit with core dump' },
  { id: 'STOP', label: 'SIGSTOP', hint: 'Pause the process' },
  { id: 'CONT', label: 'SIGCONT', hint: 'Resume a paused process' },
  { id: 'USR1', label: 'SIGUSR1', hint: 'User-defined signal 1' },
  { id: 'USR2', label: 'SIGUSR2', hint: 'User-defined signal 2' },
]

/** Runs fn; on a permission error offers to retry through sudo (never on Windows). Resolves true on success. */
async function withSudoRetry(what: string, fn: (sudo: boolean) => Promise<unknown>, sudo: boolean, windows: boolean): Promise<boolean> {
  try {
    await fn(sudo)
    return true
  } catch (err) {
    if (isApiError(err) && err.code === 'sudo_cancelled') {
      toast.info('Cancelled', { description: 'The sudo password prompt was dismissed.' })
      return false
    }
    if (isApiError(err) && err.status === 403 && err.code === 'permission_denied' && !sudo && !windows) {
      const retry = await confirm({
        title: 'Permission denied',
        description: `${err.message.replace(/\.?\s*$/, '.')} Retry ${what} with sudo? NexTerm asks for the sudo password when the host needs one.`,
        confirmLabel: 'Retry with sudo',
      })
      if (!retry) return false
      return withSudoRetry(what, fn, true, windows)
    }
    toast.error(`Could not ${what}`, { description: errorMessage(err) })
    return false
  }
}

export interface ActionOpts {
  sudo?: boolean
  windows?: boolean
  /** Ask first (default true for destructive signals). */
  confirm?: boolean
}

function procLabel(p: Pick<ProcessInfo, 'pid' | 'name' | 'command'>): string {
  const name = p.name || p.command.split(/\s+/)[0]?.split('/').pop() || 'process'
  return `${name} (PID ${p.pid})`
}

/** Send a signal to a process (confirmation for TERM / KILL / STOP / INT / QUIT). */
export async function signalProcess(target: TargetId, p: Pick<ProcessInfo, 'pid' | 'name' | 'command'>, signal: string, opts: ActionOpts = {}): Promise<boolean> {
  const destructive = ['TERM', 'KILL', 'INT', 'QUIT', 'STOP'].includes(signal)
  if (opts.confirm ?? destructive) {
    const ok = await confirm({
      title: signal === 'KILL' ? `Kill ${procLabel(p)}?` : signal === 'TERM' ? `End ${procLabel(p)}?` : `Send SIG${signal} to ${procLabel(p)}?`,
      description: p.command.length > 300 ? `${p.command.slice(0, 300)}…` : p.command,
      confirmLabel: signal === 'KILL' ? 'Kill' : signal === 'TERM' ? 'End process' : `Send SIG${signal}`,
      destructive: signal === 'KILL' || signal === 'TERM',
    })
    if (!ok) return false
  }
  const done = await withSudoRetry(`signal ${procLabel(p)}`, (sudo) => killProcess(target, p.pid, signal, sudo), !!opts.sudo, !!opts.windows)
  if (done) {
    toast.success(`SIG${signal} sent to ${procLabel(p)}`)
    void queryClient.invalidateQueries({ queryKey: monitorKeys.processes(target) })
  }
  return done
}

/** Change a process's nice value (asks for the value). */
export async function reniceWithPrompt(target: TargetId, p: Pick<ProcessInfo, 'pid' | 'name' | 'command' | 'nice'>, opts: ActionOpts = {}): Promise<boolean> {
  const value = await prompt({
    title: `Priority of ${procLabel(p)}`,
    description: opts.windows
      ? 'Nice value −20…19, mapped to Windows priority classes (High ≤ −15, Above normal < 0, Normal 0, Below normal > 0, Idle ≥ 15).'
      : 'Nice value from −20 (highest priority) to 19 (lowest). Raising the priority usually needs sudo.',
    label: 'Nice',
    defaultValue: String(p.nice ?? 0),
    type: 'number',
    confirmLabel: 'Apply',
    validate: (v) => {
      const n = Number(v)
      return Number.isInteger(n) && n >= -20 && n <= 19 ? null : 'Enter a whole number between −20 and 19'
    },
  })
  if (value == null) return false
  const nice = Number(value)
  const done = await withSudoRetry(`renice ${procLabel(p)}`, (sudo) => reniceProcess(target, p.pid, nice, sudo), !!opts.sudo, !!opts.windows)
  if (done) {
    toast.success(`${procLabel(p)} now runs at nice ${nice}`)
    void queryClient.invalidateQueries({ queryKey: monitorKeys.processes(target) })
  }
  return done
}

const ACTION_LABEL: Record<ServiceAction, string> = {
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  reload: 'Reload',
  enable: 'Enable',
  disable: 'Disable',
}

/** Start / stop / … a service (stop and disable ask first). */
export async function runServiceAction(target: TargetId, name: string, action: ServiceAction, opts: ActionOpts = {}): Promise<boolean> {
  if (action === 'stop' || action === 'disable' || action === 'restart') {
    const ok = await confirm({
      title: `${ACTION_LABEL[action]} ${name}?`,
      description:
        action === 'stop'
          ? 'Stopping a service can interrupt users or other services relying on it.'
          : action === 'disable'
            ? 'The service will no longer start at boot.'
            : 'The service is stopped and started again.',
      confirmLabel: ACTION_LABEL[action],
      destructive: action !== 'restart',
    })
    if (!ok) return false
  }
  const id = toast.loading(`${ACTION_LABEL[action]} ${name}…`)
  const done = await withSudoRetry(`${action} ${name}`, (sudo) => serviceAction(target, name, action, sudo), !!opts.sudo, !!opts.windows)
  toast.dismiss(id)
  if (done) {
    toast.success(`${name}: ${action} done`)
  }
  void queryClient.invalidateQueries({ queryKey: monitorKeys.services(target) })
  return done
}

export { ACTION_LABEL }
