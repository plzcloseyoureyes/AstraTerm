/*
 * Helpers of the keys feature: file downloads, key labels, certificate expiry texts and OpenSSH's "drunken bishop"
 * randomart (sshkey_fingerprint_randomart), rendered from the SHA256 fingerprint exactly like `ssh-keygen -lv`.
 */
import { toast } from 'sonner'
import { copyText, errorMessage } from '@/lib/utils'
import type { CertificateInfo } from './types'

/** Save text as a file through the browser. */
export function downloadText(content: string, filename: string, mime = 'text/plain'): void {
  const blob = new Blob([content], { type: mime || 'text/plain' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 30_000)
}

/** Copy with a toast. */
export async function copyWithToast(text: string, what = 'Copied to clipboard'): Promise<void> {
  if (await copyText(text)) toast.success(what)
  else toast.error('The browser refused access to the clipboard')
}

export function toastError(title: string, err: unknown): void {
  toast.error(title, { description: errorMessage(err) })
}

/** "ED25519", "RSA 4096", "ECDSA 384". */
export function keyTypeLabel(type: string, bits?: number): string {
  const t = type.toUpperCase()
  if (!bits || t === 'ED25519') return t
  return `${t} ${bits}`
}

/** "SHA256:abcd…wxyz" for dense tables. */
export function shortFingerprint(fp: string, keep = 10): string {
  const [alg, rest] = fp.includes(':') ? [fp.slice(0, fp.indexOf(':') + 1), fp.slice(fp.indexOf(':') + 1)] : ['', fp]
  if (rest.length <= keep * 2 + 1) return fp
  return `${alg}${rest.slice(0, keep)}…${rest.slice(-keep)}`
}

/** Human duration between now and t ("3 d 4 h", "25 min"). */
function spanText(ms: number): string {
  const abs = Math.abs(ms)
  const min = Math.round(abs / 60_000)
  if (min < 1) return 'less than a minute'
  if (min < 60) return `${min} min`
  const h = Math.floor(min / 60)
  if (h < 48) return `${h} h ${min % 60} min`
  const d = Math.floor(h / 24)
  if (d < 60) return `${d} d ${h % 24} h`
  if (d < 730) return `${Math.round(d / 30.4)} months`
  return `${Math.round(d / 365)} years`
}

/** Status line of a certificate: "Valid · expires in 3 d 4 h", "Expired 2 h ago", "Not valid before …". */
export function certExpiryText(info: CertificateInfo, now = Date.now()): string {
  if (info.status === 'not_yet_valid' && info.validAfter) return `Not valid for ${spanText(Date.parse(info.validAfter) - now)}`
  if (!info.validBefore) return 'Never expires'
  const left = Date.parse(info.validBefore) - now
  if (left <= 0) return `Expired ${spanText(left)} ago`
  return `Expires in ${spanText(left)}`
}

export function certTone(info: CertificateInfo, now = Date.now()): 'success' | 'warning' | 'destructive' {
  if (info.status !== 'valid') return 'destructive'
  if (info.validBefore && Date.parse(info.validBefore) - now < 24 * 3600_000) return 'warning'
  return 'success'
}

/** Default comment of generated keys, like PuTTYgen ("ed25519-key-20260927"). */
export function defaultKeyComment(type: string, d = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${type}-key-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}`
}

// ---- randomart ------------------------------------------------------------------------------------------------------

const AUGMENT = ' .o+=*BOX@%&#/^SE'
const FLD_X = 17
const FLD_Y = 9

function decodeBase64(s: string): Uint8Array | null {
  try {
    const padded = s + '='.repeat((4 - (s.length % 4)) % 4)
    const bin = atob(padded)
    const out = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
    return out
  } catch {
    return null
  }
}

/**
 * OpenSSH's randomart ("drunken bishop") for a "SHA256:…" fingerprint. `title` is the "[TYPE BITS]" header text
 * (e.g. "ED25519 256"). Returns null for malformed fingerprints.
 */
export function randomart(fingerprint: string, title: string): string | null {
  const m = /^SHA256:([A-Za-z0-9+/]+)=*$/.exec(fingerprint.trim())
  const digest = m ? decodeBase64(m[1]) : null
  if (!digest || digest.length !== 32) return null
  const len = AUGMENT.length - 1
  const field: number[][] = Array.from({ length: FLD_X }, () => Array.from({ length: FLD_Y }, () => 0))
  let x = Math.floor(FLD_X / 2)
  let y = Math.floor(FLD_Y / 2)
  for (const byte of digest) {
    let input = byte
    for (let b = 0; b < 4; b++) {
      x += input & 0x1 ? 1 : -1
      y += input & 0x2 ? 1 : -1
      x = Math.min(Math.max(x, 0), FLD_X - 1)
      y = Math.min(Math.max(y, 0), FLD_Y - 1)
      if (field[x][y] < len - 2) field[x][y]++
      input >>= 2
    }
  }
  field[Math.floor(FLD_X / 2)][Math.floor(FLD_Y / 2)] = len - 1
  field[x][y] = len

  let head = `[${title}]`
  if (head.length > FLD_X) head = `[${title.split(' ')[0]}]`
  if (head.length > FLD_X) head = ''
  const hash = '[SHA256]'
  const border = (label: string) => {
    const left = Math.floor((FLD_X - label.length) / 2)
    return '+' + '-'.repeat(left) + label + '-'.repeat(FLD_X - left - label.length) + '+'
  }
  const rows = [border(head)]
  for (let yy = 0; yy < FLD_Y; yy++) {
    let row = '|'
    for (let xx = 0; xx < FLD_X; xx++) row += AUGMENT[Math.min(field[xx][yy], len)]
    rows.push(row + '|')
  }
  rows.push(border(hash))
  return rows.join('\n')
}

/** The randomart header of a key: "ED25519 256", "RSA 4096". */
export function randomartTitle(type: string, bits: number): string {
  return `${type.toUpperCase()} ${bits}`
}
