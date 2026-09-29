/* oxlint-disable no-control-regex -- control characters are what this module encodes */
/*
 * C-style escapes shared by the button bar, trigger "send" actions, logon actions and the macro editor (the backend
 * decodes the same set in internal/automation/util.go Unescape):
 *   \r \n \t \e (ESC) \a \b \f \v \0 \\ \xHH \uHHHH
 */

export function unescapeText(s: string): string {
  if (!s.includes('\\')) return s
  let out = ''
  for (let i = 0; i < s.length; i++) {
    const c = s[i]
    if (c !== '\\' || i + 1 >= s.length) {
      out += c
      continue
    }
    const n = s[i + 1]
    switch (n) {
      case 'r':
        out += '\r'
        break
      case 'n':
        out += '\n'
        break
      case 't':
        out += '\t'
        break
      case 'e':
      case 'E':
        out += '\x1b'
        break
      case 'a':
        out += '\x07'
        break
      case 'b':
        out += '\b'
        break
      case 'f':
        out += '\f'
        break
      case 'v':
        out += '\v'
        break
      case '0':
        out += '\0'
        break
      case '\\':
        out += '\\'
        break
      case 'x': {
        const hex = s.slice(i + 2, i + 4)
        if (/^[0-9a-fA-F]{2}$/.test(hex)) {
          out += String.fromCharCode(parseInt(hex, 16))
          i += 3
          continue
        }
        out += '\\x'
        break
      }
      case 'u': {
        const hex = s.slice(i + 2, i + 6)
        if (/^[0-9a-fA-F]{4}$/.test(hex)) {
          out += String.fromCharCode(parseInt(hex, 16))
          i += 5
          continue
        }
        out += '\\u'
        break
      }
      default:
        out += '\\' + n
    }
    i++
  }
  return out
}

const NAMED: Record<string, string> = { '\r': '\\r', '\n': '\\n', '\t': '\\t', '\x1b': '\\e', '\\': '\\\\', '\0': '\\0' }

/** Make control characters visible and editable (inverse of unescapeText). */
export function escapeText(s: string): string {
  return s.replace(/[\x00-\x1f\x7f\\]/g, (c) => NAMED[c] ?? `\\x${c.charCodeAt(0).toString(16).padStart(2, '0')}`)
}

/** Human-friendly rendering of a keystroke chunk for lists (arrows, Enter, Ctrl+X…). */
export function describeKeys(s: string): string {
  const KEYS: [string, string][] = [
    ['\x1b[A', '↑'],
    ['\x1b[B', '↓'],
    ['\x1b[C', '→'],
    ['\x1b[D', '←'],
    ['\x1bOA', '↑'],
    ['\x1bOB', '↓'],
    ['\x1bOC', '→'],
    ['\x1bOD', '←'],
    ['\x1b[H', 'Home'],
    ['\x1b[F', 'End'],
    ['\x1b[3~', 'Del'],
    ['\x1b[5~', 'PgUp'],
    ['\x1b[6~', 'PgDn'],
    ['\r', '⏎'],
    ['\t', '⇥'],
    ['\x7f', '⌫'],
    ['\x1b', 'Esc'],
  ]
  let out = ''
  let i = 0
  outer: while (i < s.length) {
    for (const [seq, label] of KEYS) {
      if (s.startsWith(seq, i)) {
        out += `[${label}]`
        i += seq.length
        continue outer
      }
    }
    const code = s.charCodeAt(i)
    if (code < 0x20) out += `[Ctrl+${String.fromCharCode(code + 64)}]`
    else out += s[i]
    i++
  }
  return out
}
