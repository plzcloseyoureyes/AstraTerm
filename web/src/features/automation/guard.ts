/*
 * Dangerous-command guard (SEC-21), browser side. The backend enforces the same rules on every server-side send
 * (internal/automation/guard.go — keep both lists in sync) and answers 409 {code:'dangerous_command', matches}.
 */
import type { Connection } from '@/api/types'
import { automationSettings } from './settings'
import type { DangerMatch, GuardCustomRule } from './types'

interface Rule {
  id: string
  message: string
  severity: 'danger' | 'warning'
  re: RegExp
}

const RULES: Rule[] = [
  {
    id: 'rm-root',
    message: 'Recursively deletes the root, home or every file (rm -r on / ~ *).',
    severity: 'danger',
    re: /\brm\s+(?:-[-\w]+\s+)*-[a-z]*r[a-z]*\s+(?:-[-\w]+\s+)*(?:\/\*?|~\/?|\*|\$home\/?|\.\.?\/?\*?)(?:\s|$|[;&|)])|\brm\b[^\n]*--no-preserve-root/i,
  },
  { id: 'rm-rf', message: 'Recursively force-deletes files (rm -rf).', severity: 'warning', re: /\brm\s+(?:-[-\w]+\s+)*-(?:[a-z]*r[a-z]*f|[a-z]*f[a-z]*r)[a-z]*\b/i },
  {
    id: 'mkfs',
    message: 'Creates a filesystem / wipes a device (mkfs, mkswap, wipefs).',
    severity: 'danger',
    re: /(?:^|[\s;&|(])(?:mkfs(?:\.\w+)?|mke2fs|mkswap|wipefs|mkfs_msdos)\s/i,
  },
  { id: 'dd-dev', message: 'Writes raw data to a device (dd of=/dev/…).', severity: 'danger', re: /\bdd\b[^\n]*\bof=\/dev\/(?:sd|hd|vd|xvd|nvme|mmcblk|disk|rdisk|md|dm-|mapper|loop)/i },
  { id: 'redirect-dev', message: 'Overwrites a block device (> /dev/sdX).', severity: 'danger', re: />\s*\/dev\/(?:sd|hd|vd|xvd|nvme|mmcblk|disk|rdisk)\w*/i },
  { id: 'partition', message: 'Rewrites a partition table.', severity: 'warning', re: /(?:^|[\s;&|(])(?:fdisk|sfdisk|gdisk|sgdisk|parted)\s+(?:-[-\w]+\s+)*\/dev\//i },
  { id: 'fork-bomb', message: 'Fork bomb.', severity: 'danger', re: /:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:/ },
  {
    id: 'power',
    message: 'Shuts down or reboots the machine.',
    severity: 'danger',
    re: /(?:^|[;&|(]|\bsudo\s+|\bexec\s+|\bdoas\s+)\s*(?:shutdown|reboot|halt|poweroff|init\s+[06]|telinit\s+[06]|systemctl\s+(?:reboot|poweroff|halt|kexec|emergency|rescue))\b/i,
  },
  {
    id: 'chmod-root',
    message: 'Recursively changes permissions / ownership from the root directory.',
    severity: 'danger',
    re: /\b(?:chmod|chown|chgrp)\s+(?:-[-\w]+\s+)*-[a-z]*R[a-z]*\s+\S+\s+\/(?:\s|$)/i,
  },
  { id: 'kill-all', message: 'Kills every process.', severity: 'warning', re: /\bkill\s+(?:-\S+\s+)*-1\b|\bkillall5\b|\bpkill\s+(?:-\S+\s+)*-u\s+root\b/i },
  {
    id: 'firewall-flush',
    message: 'Flushes / disables the firewall (may cut your own access).',
    severity: 'warning',
    re: /\b(?:iptables|ip6tables)\s+(?:-\S+\s+)*-F\b|\bnft\s+flush\s+ruleset\b|\bufw\s+disable\b/i,
  },
  {
    id: 'iface-down',
    message: 'Takes a network interface down (may cut your own access).',
    severity: 'warning',
    re: /\bip\s+link\s+set\s+\S+\s+down\b|\bifconfig\s+\S+\s+down\b|\bifdown\s+\S+/i,
  },
  { id: 'sql-drop', message: 'Drops a database or schema.', severity: 'danger', re: /\bdrop\s+(?:database|schema)\b/i },
  { id: 'sql-table', message: 'Drops or truncates a table.', severity: 'warning', re: /\bdrop\s+table\b|\btruncate\s+(?:table\s+)?\w/i },
  { id: 'sql-delete-all', message: 'DELETE without WHERE removes every row.', severity: 'warning', re: /\bdelete\s+from\s+[\w."`[\]]+\s*(?:;|$)/i },
  {
    id: 'cisco',
    message: 'Reloads / erases a network device.',
    severity: 'danger',
    re: /^\s*(?:reload(?:\s+in\s+\d+)?|write\s+erase|erase\s+(?:startup-config|nvram:|flash:)|format\s+(?:flash|disk\d?|bootflash):)\s*$/i,
  },
  {
    id: 'windows-format',
    message: 'Formats a drive / deletes recursively on Windows.',
    severity: 'danger',
    re: /\bformat\s+[a-z]:|\bRemove-Item\b[^\n]*-Recurse[^\n]*\s[a-z]:\\?(?:\s|$)|\brd\s+\/s\s+\/q\s+[a-z]:\\?(?:\s|$)/i,
  },
  { id: 'crontab-remove', message: 'Removes the crontab.', severity: 'warning', re: /\bcrontab\s+(?:-u\s+\S+\s+)?-r\b/i },
  { id: 'k8s-delete', message: 'Deletes Kubernetes namespaces / everything.', severity: 'warning', re: /\bkubectl\s+delete\s+(?:ns|namespaces?|all)\b|\bkubectl\s+delete\b[^\n]*--all\b/i },
  { id: 'terraform-destroy', message: 'Destroys infrastructure.', severity: 'warning', re: /\bterraform\s+destroy\b|\bpulumi\s+destroy\b/i },
]

const customCache = new Map<string, RegExp | null>()

function compileCustom(p: string): RegExp | null {
  if (customCache.has(p)) return customCache.get(p) ?? null
  let re: RegExp | null = null
  try {
    re = new RegExp(p, 'i')
  } catch {
    re = null
  }
  customCache.set(p, re)
  return re
}

/** Split text into logical command lines (joining backslash continuations). */
export function commandLines(text: string): string[] {
  const out: string[] = []
  let cur = ''
  const lines = text.replace(/\r\n?/g, '\n').split('\n')
  if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop()
  for (const l of lines) {
    if (l.endsWith('\\')) {
      cur += l.slice(0, -1) + ' '
      continue
    }
    out.push(cur + l)
    cur = ''
  }
  if (cur) out.push(cur)
  return out
}

export interface GuardOptions {
  enabled?: boolean
  strict?: boolean
  custom?: GuardCustomRule[]
}

/** Guard hits of text (one per rule). Uses the user's settings unless overridden. */
export function checkDangerous(text: string, opts: GuardOptions = {}): DangerMatch[] {
  const s = automationSettings.get()
  const enabled = opts.enabled ?? s.guardEnabled
  const strict = opts.strict ?? s.guardStrict
  const custom = opts.custom ?? s.guardCustom
  if (!enabled || !text.trim()) return []
  const rules: Rule[] = [...RULES]
  for (const c of custom.slice(0, 100)) {
    const re = c.pattern?.trim() ? compileCustom(c.pattern) : null
    if (re) rules.push({ id: 'custom', message: c.message?.trim() || 'Matches a custom dangerous-command rule.', severity: 'danger', re })
  }
  const out: DangerMatch[] = []
  const seen = new Set<string>()
  for (let line of commandLines(text)) {
    if (line.length > 8192) line = line.slice(0, 8192)
    for (const r of rules) {
      if (r.severity === 'warning' && !strict) continue
      const key = r.id + '\0' + r.message
      if (seen.has(key) || !r.re.test(line)) continue
      seen.add(key)
      out.push({ rule: r.id, message: r.message, severity: r.severity, line: line.trim().slice(0, 300) })
    }
  }
  return out
}

/** Interpret keystrokes as a line editor would (macros, typed input): Backspace, Ctrl-U/C, escape sequences, Enter. */
export function typedText(data: string): string {
  let out = ''
  let line: string[] = []
  const chars = Array.from(data)
  for (let i = 0; i < chars.length; i++) {
    const c = chars[i]
    const code = c.codePointAt(0) ?? 0
    if (c === '\r' || c === '\n') {
      out += line.join('') + '\n'
      line = []
    } else if (code === 0x7f || code === 0x08) {
      line.pop()
    } else if (code === 0x15 || code === 0x03) {
      line = []
    } else if (code === 0x1b) {
      if (chars[i + 1] === '[' || chars[i + 1] === 'O') {
        i += 2
        while (i < chars.length && !(chars[i] >= '@' && chars[i] <= '~')) i++
      } else {
        i++
      }
    } else if (c === '\t') {
      line.push(' ')
    } else if (code >= 0x20) {
      line.push(c)
    }
  }
  return out + line.join('')
}

/** Is a connection production-tagged (settings.productionTags, case-insensitive)? */
export function isProduction(conn: Pick<Connection, 'tags'> | undefined): boolean {
  if (!conn?.tags?.length) return false
  const tags = new Set(automationSettings.get().productionTags.map((t) => t.toLowerCase()))
  return conn.tags.some((t) => tags.has(t.toLowerCase()))
}
