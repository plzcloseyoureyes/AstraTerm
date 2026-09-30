/*
 * Dangerous-command guard (SEC-21) for text the terminal feature itself sends on the user's behalf — the MultiExec
 * compose line. Typed input in terminals is guarded by the automation module's terminal plugin; the compose line
 * bypasses the terminals' input path, so it is checked here against the same server rules
 * (POST /api/automation/guard/check). Without the automation module (404) or on a failed check the text is sent:
 * the guard is a safety net, not an access control.
 */
import { createElement } from 'react'
import { api, isApiError } from '@/api/client'
import { confirm } from '@/components/ui/dialog-host'

interface DangerMatch {
  rule: string
  message: string
  severity: 'danger' | 'warning' | string
  line: string
}

async function checkDangerous(text: string): Promise<DangerMatch[]> {
  try {
    const res = await api.post<{ enabled: boolean; matches: DangerMatch[] | null }>('/api/automation/guard/check', { text }, { noVaultPrompt: true })
    return res.enabled ? (res.matches ?? []) : []
  } catch (err) {
    if (!isApiError(err) || err.status !== 404) console.warn('[terminal] dangerous-command check failed', err)
    return []
  }
}

/** True when `text` may be sent to `targets` terminals: no guard match, or the user confirmed. */
export async function confirmIfDangerous(text: string, targets: number): Promise<boolean> {
  const matches = await checkDangerous(text)
  if (!matches.length) return true
  const where = targets === 1 ? 'one terminal' : `${targets} terminals`
  return confirm({
    title: `Send a dangerous command to ${where}?`,
    description: createElement(
      'ul',
      { className: 'grid gap-1.5' },
      matches.map((m, i) =>
        createElement(
          'li',
          { key: `${m.rule}-${i}` },
          createElement('span', { className: m.severity === 'danger' ? 'font-medium text-destructive' : 'font-medium text-warning' }, m.message),
          m.line ? createElement('code', { className: 'mt-0.5 block truncate font-mono text-xs text-muted-foreground' }, m.line) : null,
        ),
      ),
    ),
    confirmLabel: 'Send anyway',
    destructive: true,
  })
}
