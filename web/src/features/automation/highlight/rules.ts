/*
 * Keyword highlighting rule sets (TERM-15): errors red, warnings yellow, success green, addresses,
 * URLs, dates; optional numbers, paths, SQL keywords. Colours are ANSI names resolved against the terminal's colour
 * scheme (so they suit every theme) or #rrggbb.
 */
import type { AnsiColor } from '../types'

export interface RuleDef {
  re: RegExp
  color?: AnsiColor | string
  background?: AnsiColor | string
  underline?: boolean
}

export interface RuleSet {
  id: string
  name: string
  description: string
  sample: string
  rules: RuleDef[]
}

const IPV4 = String.raw`(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)`
const H16 = '[0-9a-fA-F]{1,4}'
const IPV6 = [
  `(?:${H16}:){7}${H16}`,
  `(?:${H16}:){1,7}:`,
  `(?:${H16}:){1,6}:${H16}`,
  `(?:${H16}:){1,5}(?::${H16}){1,2}`,
  `(?:${H16}:){1,4}(?::${H16}){1,3}`,
  `(?:${H16}:){1,3}(?::${H16}){1,4}`,
  `(?:${H16}:){1,2}(?::${H16}){1,5}`,
  `${H16}:(?::${H16}){1,6}`,
  `:(?::${H16}){1,7}`,
  `::`,
].join('|')

export const RULE_SETS: RuleSet[] = [
  {
    id: 'errors',
    name: 'Errors',
    description: 'error, failed, denied, refused, fatal, exception…',
    sample: 'ERROR: Permission denied (publickey)',
    rules: [
      {
        re: /\b(?:errors?|err|fail(?:ed|ure|ures|s)?|fatal|denied|refused|critical|crit|emerg(?:ency)?|panic|exceptions?|traceback|segmentation fault|segfault|abort(?:ed)?|invalid|unreachable|timed?[ -]?out|not found|no such (?:file|directory|host)|cannot|can't|unable to|forbidden|unauthori[sz]ed|rejected|corrupt(?:ed)?|violation|killed|oom)\b/gi,
        color: 'brightRed',
      },
    ],
  },
  {
    id: 'warnings',
    name: 'Warnings',
    description: 'warning, deprecated, retrying, degraded…',
    sample: 'WARNING: deprecated option, retrying',
    rules: [{ re: /\b(?:warn(?:ing|ings)?|deprecated|caution|notice|retry(?:ing)?|degraded|pending|expired?|expiring)\b/gi, color: 'brightYellow' }],
  },
  {
    id: 'success',
    name: 'Success',
    description: 'ok, success, passed, done, enabled, connected…',
    sample: 'Build succeeded — all tests passed [OK]',
    rules: [
      {
        re: /\b(?:ok|okay|success(?:ful(?:ly)?)?|succeeded|passed|done|complete(?:d)?|enabled|active|running|online|ready|accepted|connected|established|healthy|listening|up-to-date)\b/gi,
        color: 'brightGreen',
      },
    ],
  },
  {
    id: 'levels',
    name: 'Log levels',
    description: 'INFO, DEBUG, TRACE in log files',
    sample: '2026-09-27 12:00:01 INFO started · DEBUG cache miss',
    rules: [
      { re: /\b(?:INFO|NOTICE)\b/g, color: 'brightCyan' },
      { re: /\b(?:DEBUG|TRACE|VERBOSE)\b/g, color: 'brightBlack' },
    ],
  },
  {
    id: 'network',
    name: 'Network devices',
    description: 'interfaces, up / down, err-disabled (Cisco, Juniper…)',
    sample: 'GigabitEthernet0/1 is up, line protocol is down (err-disabled)',
    rules: [
      { re: /\b(?:administratively down|err-disabled|notconnect|down)\b/gi, color: 'brightRed' },
      { re: /\b(?:up|connected|forwarding)\b/gi, color: 'brightGreen' },
      { re: /\b(?:blocking|listening|learning|half-duplex)\b/gi, color: 'brightYellow' },
      {
        re: /\b(?:(?:Ten|Forty|Hundred|Twenty[Ff]ive)?GigabitEthernet|FastEthernet|Ethernet|Port-channel|Vlan|Loopback|Tunnel|Gi|Te|Fa|Et|Eth|Po|Lo|Tu|xe|ge|et|ae)-?\d+(?:\/\d+)*(?:\.\d+)?\b/g,
        color: 'brightMagenta',
      },
    ],
  },
  {
    id: 'ipv4',
    name: 'IPv4 addresses',
    description: '192.168.1.10, 10.0.0.0/8, 1.2.3.4:22',
    sample: 'Connection from 192.168.1.10:51234 to 10.0.0.0/8',
    rules: [{ re: new RegExp(String.raw`(?<![\w.])${IPV4}(?:\/\d{1,2})?(?::\d{1,5})?(?![\w.])`, 'g'), color: 'brightBlue' }],
  },
  {
    id: 'ipv6',
    name: 'IPv6 addresses',
    description: 'fe80::1, 2001:db8::/32',
    sample: 'inet6 fe80::1c2b:3aff:fe4d:5e6f/64 scope link',
    rules: [{ re: new RegExp(String.raw`(?<![\w:.])(?:${IPV6})(?:\/\d{1,3})?(?![\w:])`, 'g'), color: 'brightBlue' }],
  },
  {
    id: 'mac',
    name: 'MAC addresses',
    description: '00:1a:2b:3c:4d:5e, 001a.2b3c.4d5e',
    sample: 'link/ether 00:1a:2b:3c:4d:5e brd ff:ff:ff:ff:ff:ff',
    rules: [{ re: /(?<![\w:.-])(?:[0-9a-fA-F]{2}(?:[:-][0-9a-fA-F]{2}){5}|[0-9a-fA-F]{4}\.[0-9a-fA-F]{4}\.[0-9a-fA-F]{4})(?![\w:.-])/g, color: 'brightMagenta' }],
  },
  {
    id: 'url',
    name: 'URLs',
    description: 'http(s), ftp, ssh and file links',
    sample: 'See https://example.com/docs?page=2 for details',
    rules: [{ re: /\b(?:https?|ftps?|sftp|ssh|file|wss?):\/\/[^\s<>"'`]+[^\s<>"'`.,;:!?)\]}]/g, color: 'brightBlue', underline: true }],
  },
  {
    id: 'datetime',
    name: 'Dates & times',
    description: 'ISO dates, syslog timestamps, clock times',
    sample: 'Sep 27 12:34:56 host sshd[42] · 2026-09-27T12:34:56Z',
    rules: [
      {
        re: /\b(?:\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?|(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}|\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?)\b/g,
        color: 'cyan',
      },
    ],
  },
  {
    id: 'numbers',
    name: 'Numbers',
    description: 'integers, decimals, sizes (12, 3.5, 1.2G, 512MiB)',
    sample: 'used 1.2G of 16G (7.5%), 512 MiB free',
    rules: [{ re: /(?<![\w.])\d+(?:\.\d+)?(?:\s?(?:[kKMGTP]i?B?|%|ms|s))?(?![\w.])/g, color: 'yellow' }],
  },
  {
    id: 'paths',
    name: 'File paths',
    description: '/etc/nginx/nginx.conf, ~/projects, ./build',
    sample: 'Edited /etc/nginx/nginx.conf and ~/notes/todo.md',
    rules: [{ re: /(?<![\w/.~-])(?:~|\.{1,2})?(?:\/[\w.@+-]+)+\/?/g, color: 'green' }],
  },
  {
    id: 'sql',
    name: 'SQL keywords',
    description: 'SELECT, INSERT, UPDATE, JOIN, WHERE…',
    sample: 'SELECT id FROM users WHERE active = 1 ORDER BY id',
    rules: [
      {
        re: /\b(?:SELECT|INSERT|UPDATE|DELETE|FROM|WHERE|JOIN|LEFT|RIGHT|INNER|OUTER|ON|GROUP BY|ORDER BY|HAVING|LIMIT|OFFSET|VALUES|INTO|CREATE|ALTER|DROP|TABLE|INDEX|VIEW|AND|OR|NOT|NULL|IS|IN|AS|DISTINCT|UNION|BEGIN|COMMIT|ROLLBACK)\b/g,
        color: 'brightBlue',
      },
    ],
  },
]

export const ANSI_COLORS: { id: string; label: string }[] = [
  { id: 'red', label: 'Red' },
  { id: 'brightRed', label: 'Bright red' },
  { id: 'green', label: 'Green' },
  { id: 'brightGreen', label: 'Bright green' },
  { id: 'yellow', label: 'Yellow' },
  { id: 'brightYellow', label: 'Bright yellow' },
  { id: 'blue', label: 'Blue' },
  { id: 'brightBlue', label: 'Bright blue' },
  { id: 'magenta', label: 'Magenta' },
  { id: 'brightMagenta', label: 'Bright magenta' },
  { id: 'cyan', label: 'Cyan' },
  { id: 'brightCyan', label: 'Bright cyan' },
  { id: 'white', label: 'White' },
  { id: 'brightBlack', label: 'Grey' },
  { id: 'black', label: 'Black' },
]

/** Fallback palette (xterm defaults) when the terminal theme does not give a #rrggbb colour. */
export const FALLBACK_ANSI: Record<string, string> = {
  black: '#2e3436',
  red: '#cc0000',
  green: '#4e9a06',
  yellow: '#c4a000',
  blue: '#3465a4',
  magenta: '#75507b',
  cyan: '#06989a',
  white: '#d3d7cf',
  brightBlack: '#555753',
  brightRed: '#ef2929',
  brightGreen: '#8ae234',
  brightYellow: '#fce94f',
  brightBlue: '#729fcf',
  brightMagenta: '#ad7fa8',
  brightCyan: '#34e2e2',
  brightWhite: '#eeeeec',
}

/** Compile a user pattern (JavaScript syntax) into a global regex, or null when invalid / empty-matching. */
export function compileUserPattern(pattern: string, caseSensitive?: boolean): RegExp | null {
  if (!pattern?.trim() || pattern.length > 2000) return null
  try {
    const re = new RegExp(pattern, caseSensitive ? 'gu' : 'giu')
    if (re.test('')) return null // matches the empty string: would loop / highlight nothing
    re.lastIndex = 0
    return re
  } catch {
    try {
      const re = new RegExp(pattern, caseSensitive ? 'g' : 'gi')
      if (re.test('')) return null
      re.lastIndex = 0
      return re
    } catch {
      return null
    }
  }
}

/** Resolve a colour value (ANSI name or #hex) to CSS for previews in the UI. */
export function cssColor(c: string | undefined): string | undefined {
  if (!c) return undefined
  if (c.startsWith('#')) return c
  return FALLBACK_ANSI[c]
}
