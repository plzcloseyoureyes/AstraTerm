/*
 * Formatting and classification of file entries: sizes, dates, permission strings / octal modes, file kinds (icons,
 * preview and "open" behaviour), archive formats.
 */
import { format as formatDate } from 'date-fns'
import type { FileEntry } from '@/api/types'
import { extname } from './paths'

// ---------------------------------------------------------------------------------------------------------------------
// entries
// ---------------------------------------------------------------------------------------------------------------------

/** A directory, or a symlink pointing at one (navigates like a folder). */
export function isDirLike(e: Pick<FileEntry, 'type' | 'linkType'>): boolean {
  return e.type === 'dir' || (e.type === 'symlink' && e.linkType === 'dir')
}

export function isSymlink(e: Pick<FileEntry, 'type'>): boolean {
  return e.type === 'symlink'
}

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']

/** Compact size for narrow columns: "35 B", "4.5 KiB", "20.0 MiB", "300 MiB". */
export function formatBytesCompact(bytes: number): string {
  if (!Number.isFinite(bytes)) return ''
  let v = Math.abs(bytes)
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  const num = i === 0 ? String(Math.round(v)) : v >= 100 ? v.toFixed(0) : v.toFixed(1)
  return `${bytes < 0 ? '-' : ''}${num} ${UNITS[i]}`
}

/** Size column: bytes for files, empty for folders. */
export function formatSize(e: FileEntry): string {
  if (isDirLike(e)) return ''
  return formatBytesCompact(e.size)
}

/** Exact byte count with thousands separators. */
export function formatExactBytes(n: number): string {
  return `${n.toLocaleString()} byte${n === 1 ? '' : 's'}`
}

/** "2026-09-27 14:03" (compact, sortable). */
export function formatMtime(iso: string | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime()) || d.getTime() <= 0) return ''
  return formatDate(d, 'yyyy-MM-dd HH:mm')
}

export function formatMtimeLong(iso: string | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return formatDate(d, 'yyyy-MM-dd HH:mm:ss')
}

export function mtimeMs(e: FileEntry): number {
  const t = Date.parse(e.mtime)
  return Number.isFinite(t) ? t : 0
}

// ---------------------------------------------------------------------------------------------------------------------
// permissions
// ---------------------------------------------------------------------------------------------------------------------

export const S_ISUID = 0o4000
export const S_ISGID = 0o2000
export const S_ISVTX = 0o1000

/** Permission bits (incl. setuid/setgid/sticky) of a mode that may also carry file type bits. */
export function permBits(mode: number): number {
  return (mode >>> 0) & 0o7777
}

/** "0755" */
export function modeToOctal(mode: number): string {
  return permBits(mode).toString(8).padStart(4, '0')
}

/** Parse "755" / "0755" / "4755"; null when invalid. */
export function octalToMode(text: string): number | null {
  const t = text.trim()
  if (!/^[0-7]{1,4}$/.test(t)) return null
  return parseInt(t, 8)
}

const TYPE_CHAR: Record<FileEntry['type'], string> = { dir: 'd', symlink: 'l', file: '-', other: '?' }

/** "drwxr-xr-x" from a mode (the server usually sends `perm`, this is the fallback). */
export function permString(mode: number, type: FileEntry['type'] = 'file'): string {
  const m = permBits(mode)
  const rwx = (bits: number, special: boolean, specialChar: string) => {
    const r = bits & 4 ? 'r' : '-'
    const w = bits & 2 ? 'w' : '-'
    let x = bits & 1 ? 'x' : '-'
    if (special) x = bits & 1 ? specialChar : specialChar.toUpperCase()
    return r + w + x
  }
  return (
    TYPE_CHAR[type] +
    rwx((m >> 6) & 7, !!(m & S_ISUID), 's') +
    rwx((m >> 3) & 7, !!(m & S_ISGID), 's') +
    rwx(m & 7, !!(m & S_ISVTX), 't')
  )
}

export function entryPerm(e: FileEntry): string {
  return e.perm || permString(e.mode, e.type)
}

// ---------------------------------------------------------------------------------------------------------------------
// kinds
// ---------------------------------------------------------------------------------------------------------------------

export type FileKind =
  | 'dir'
  | 'image'
  | 'video'
  | 'audio'
  | 'archive'
  | 'pdf'
  | 'code'
  | 'script'
  | 'config'
  | 'data'
  | 'text'
  | 'spreadsheet'
  | 'document'
  | 'key'
  | 'binary'
  | 'other'

const EXT_KIND: Record<string, FileKind> = {}
function add(kind: FileKind, exts: string) {
  for (const e of exts.split(/\s+/)) if (e) EXT_KIND[e] = kind
}
add('image', 'png jpg jpeg gif webp bmp ico svg avif apng tif tiff heic')
add('video', 'mp4 webm mkv mov avi m4v ogv mpg mpeg wmv flv 3gp')
add('audio', 'mp3 wav ogg oga flac m4a aac opus weba mid midi')
add('archive', 'zip tar gz tgz bz2 tbz2 xz txz zst 7z rar tar.gz tar.bz2 tar.xz tar.zst deb rpm apk jar war iso dmg cab lz lzma z')
add('pdf', 'pdf')
add(
  'code',
  'js mjs cjs jsx ts tsx go rs c h cc cpp hpp cxx java kt kts scala py rb php pl pm lua r swift m mm cs fs vb dart ex exs erl hrl hs clj cljs elm vue svelte sql graphql gql proto css scss sass less html htm xml xsl xhtml asm s zig nim v d',
)
add('script', 'sh bash zsh fish ksh csh tcsh ps1 psm1 bat cmd awk sed')
add('config', 'conf cfg ini toml yaml yml env properties service timer socket mount target rules plist reg nix tf tfvars hcl lock editorconfig gitignore gitattributes dockerignore npmrc')
add('data', 'json jsonl ndjson geojson har')
add('text', 'txt text md markdown rst log out err csv tsv adoc org nfo readme license changelog diff patch srt vtt')
add('spreadsheet', 'xls xlsx ods numbers')
add('document', 'doc docx odt rtf ppt pptx odp pages key epub')
add('key', 'pem key pub crt cer der csr p12 pfx jks gpg asc sig ppk')
add('binary', 'exe dll so dylib bin o a lib class pyc wasm img qcow2 vmdk vdi dat db sqlite sqlite3 ttf otf woff woff2 psd ai sketch')

const NAME_KIND: Record<string, FileKind> = {
  dockerfile: 'config',
  makefile: 'code',
  'cmakelists.txt': 'code',
  jenkinsfile: 'code',
  vagrantfile: 'code',
  gemfile: 'code',
  rakefile: 'code',
  procfile: 'config',
  readme: 'text',
  license: 'text',
  changelog: 'text',
  authors: 'text',
  '.bashrc': 'script',
  '.zshrc': 'script',
  '.profile': 'script',
  '.bash_profile': 'script',
  '.bash_logout': 'script',
  '.bash_history': 'text',
  '.zsh_history': 'text',
  '.vimrc': 'config',
  '.gitconfig': 'config',
  '.tmux.conf': 'config',
  '.inputrc': 'config',
  authorized_keys: 'key',
  known_hosts: 'key',
  id_rsa: 'key',
  id_ed25519: 'key',
  id_ecdsa: 'key',
  crontab: 'config',
  hosts: 'config',
  fstab: 'config',
  passwd: 'config',
  group: 'config',
  shadow: 'config',
}

export function fileKind(e: Pick<FileEntry, 'name' | 'type' | 'linkType'>): FileKind {
  if (isDirLike(e)) return 'dir'
  const lower = e.name.toLowerCase()
  const byName = NAME_KIND[lower]
  if (byName) return byName
  const ext = extname(lower)
  if (ext && EXT_KIND[ext]) return EXT_KIND[ext]
  // "archive.tar.gz" → try the last extension too.
  const last = lower.lastIndexOf('.')
  if (last > 0) {
    const k = EXT_KIND[lower.slice(last + 1)]
    if (k) return k
  }
  if (lower.startsWith('.') && lower.length > 1) return 'config'
  return 'other'
}

export type PreviewKind = 'image' | 'video' | 'audio' | 'pdf' | 'markdown' | 'text'

/** How a file can be previewed in the browser (null: download only). `other` files are sniffed as text. */
export function previewKind(e: Pick<FileEntry, 'name' | 'type' | 'linkType'>): PreviewKind | null {
  const k = fileKind(e)
  const ext = extname(e.name)
  switch (k) {
    case 'image':
      return ext === 'tif' || ext === 'tiff' || ext === 'heic' ? null : 'image'
    case 'video':
      return ['mp4', 'webm', 'ogv', 'm4v', 'mov'].includes(ext) ? 'video' : null
    case 'audio':
      return ['mp3', 'wav', 'ogg', 'oga', 'flac', 'm4a', 'aac', 'opus', 'weba'].includes(ext) ? 'audio' : null
    case 'pdf':
      return 'pdf'
    case 'text':
      return ext === 'md' || ext === 'markdown' ? 'markdown' : 'text'
    case 'code':
    case 'script':
    case 'config':
    case 'data':
    case 'key':
    case 'other':
      return 'text'
    default:
      return null
  }
}

/** Files that make no sense in a text editor (open them with preview or download instead). */
export function isBinaryKind(k: FileKind): boolean {
  return k === 'image' || k === 'video' || k === 'audio' || k === 'archive' || k === 'pdf' || k === 'spreadsheet' || k === 'document' || k === 'binary'
}

export type ArchiveFormat = 'zip' | 'tar.gz'

/** Can "Extract here" handle this file? */
export function isExtractable(name: string): boolean {
  const lower = name.toLowerCase()
  return /\.(zip|tar|tgz|tar\.gz|tar\.bz2|tbz2|tar\.xz|txz|tar\.zst|gz|bz2|xz|7z|rar|jar)$/.test(lower)
}

export function isHiddenName(name: string): boolean {
  return name.startsWith('.')
}

/** The backend receives uploads and transfers into "<target>.termstead-part" and renames it onto the target at the end. */
export const PART_SUFFIX = '.termstead-part'

/**
 * A partial upload / transfer left by Termstead ("<name>.termstead-part"): an upload in progress, or one that was
 * interrupted (page reload, cancel). Listings hide them unless settings.files.showPartialUploads is on.
 */
export function isPartialUpload(e: Pick<FileEntry, 'name' | 'type'>): boolean {
  return e.type === 'file' && e.name.length > PART_SUFFIX.length && e.name.endsWith(PART_SUFFIX)
}

/** "1 file", "30,000 files" (locale thousands separators). */
export function countOf(n: number, one: string, many = `${one}s`): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`
}

/** Sum of file sizes (folders count 0). */
export function totalSize(entries: FileEntry[]): number {
  let n = 0
  for (const e of entries) if (!isDirLike(e)) n += e.size || 0
  return n
}

/** "3 folders, 12 files" */
export function describeCounts(entries: FileEntry[]): string {
  let dirs = 0
  let files = 0
  for (const e of entries) {
    if (isDirLike(e)) dirs++
    else files++
  }
  const parts: string[] = []
  if (dirs) parts.push(countOf(dirs, 'folder'))
  if (files) parts.push(countOf(files, 'file'))
  return parts.join(', ') || 'empty'
}

/** "report.pdf" or "3 items" */
export function describeSelection(entries: FileEntry[]): string {
  if (entries.length === 1) return `“${entries[0].name}”`
  return countOf(entries.length, 'item')
}

/** Guess a MIME type (DownloadURL drag-out, previews). */
export function guessMime(name: string): string {
  const ext = extname(name)
  const map: Record<string, string> = {
    png: 'image/png',
    jpg: 'image/jpeg',
    jpeg: 'image/jpeg',
    gif: 'image/gif',
    webp: 'image/webp',
    svg: 'image/svg+xml',
    bmp: 'image/bmp',
    ico: 'image/x-icon',
    avif: 'image/avif',
    pdf: 'application/pdf',
    mp4: 'video/mp4',
    webm: 'video/webm',
    mp3: 'audio/mpeg',
    wav: 'audio/wav',
    ogg: 'audio/ogg',
    json: 'application/json',
    zip: 'application/zip',
    gz: 'application/gzip',
    'tar.gz': 'application/gzip',
    tgz: 'application/gzip',
    txt: 'text/plain',
    md: 'text/markdown',
    html: 'text/html',
    csv: 'text/csv',
  }
  return map[ext] ?? 'application/octet-stream'
}
