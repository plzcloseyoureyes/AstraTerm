// Pure helpers of the editor: codecs, save bodies, change detection, colour swatches, HTML export, theme colours,
// language detection, indentation, minimal text edits.
import assert from 'node:assert/strict'
import test from 'node:test'
import { base64ToBytes, bytesToBase64, decodeBytes, detectEol, encodeText, EncodeError, sniffBytes } from '../codec'
import { colorLiteral, findColors, literalColor } from '../colors'
import { scanBreaks } from '../eol'
import { convertIndentation, detectIndentation, linesOf, visualColumn } from '../indent'
import { detectLanguage, findLanguage, languageNames } from '../languages'
import { parseColor, toHex } from '../monaco/color'
import { ruleIndexOf, safeFontFamily, SYNTAX_RULES, tokenColor } from '../monaco/palette'
import { applyEdits, textEdits } from '../textdiff'
import { escapeHtml, renderHtml } from '../export'
import { remoteChanged } from '../filestat'
import { sameMeta, withEol, writeBody } from '../save'

test('every Windows-1252 / Latin-1 / KOI8-R byte round-trips through decode + encode', () => {
  const all = Uint8Array.from({ length: 256 }, (_, i) => i)
  for (const enc of ['windows-1252', 'iso-8859-1', 'koi8-r', 'ibm866', 'macintosh']) {
    const { text } = decodeBytes(all, enc)
    assert.deepEqual(Array.from(encodeText(text, enc, false)), Array.from(all), enc)
  }
})

test('UTF-8 / UTF-16 BOMs are detected, stripped and written back', () => {
  const u8 = Uint8Array.of(0xef, 0xbb, 0xbf, 0x68, 0x69)
  assert.deepEqual(decodeBytes(u8, 'utf-8'), { text: 'hi', bom: true })
  assert.deepEqual(Array.from(encodeText('hi', 'utf-8', true)), Array.from(u8))
  const le = Uint8Array.of(0xff, 0xfe, 0x68, 0x00, 0x3d, 0xd8, 0x00, 0xde) // "h😀"
  const d = decodeBytes(le, 'utf-16le')
  assert.equal(d.bom, true)
  assert.equal(d.text, 'h\u{1F600}')
  assert.deepEqual(Array.from(encodeText(d.text, 'utf-16le', true)), Array.from(le))
  assert.deepEqual(Array.from(encodeText('A', 'utf-16be', true)), [0xfe, 0xff, 0x00, 0x41])
})

test('unrepresentable characters and decode-only code pages raise EncodeError', () => {
  assert.throws(() => encodeText('✓', 'windows-1252', false), EncodeError)
  // encodings Termstead only decodes (not in the picker's writable list)
  assert.throws(() => encodeText('x', 'iso-2022-jp', false), (e) => e instanceof EncodeError && e.index === -1)
  const err = (() => {
    try {
      encodeText('ab€\u2713', 'iso-8859-1', false)
    } catch (e) {
      return e
    }
  })()
  assert.ok(err instanceof EncodeError)
  assert.equal(err.index, 2)
})

test('sniffBytes: text, legacy text, UTF-16 without BOM, binary', () => {
  assert.deepEqual(sniffBytes(new TextEncoder().encode('plain ascii\n')), { kind: 'text', encoding: 'utf-8' })
  assert.equal(sniffBytes(Uint8Array.of(0x63, 0x61, 0x66, 0xe9, 0x0a)).encoding, 'windows-1252')
  const u16 = new Uint8Array(40)
  for (let i = 0; i < 20; i++) u16[i * 2] = 0x61
  assert.equal(sniffBytes(u16).encoding, 'utf-16le')
  assert.equal(sniffBytes(Uint8Array.of(0x7f, 0x45, 0x4c, 0x46, 0x02, 0x01, 0x01, 0x00, 0x00)).kind, 'binary')
  assert.deepEqual(sniffBytes(new Uint8Array(0)), { kind: 'text', encoding: 'utf-8' })
})

test('base64 round trip (large, all byte values)', () => {
  const bytes = Uint8Array.from({ length: 300_000 }, (_, i) => (i * 7919) & 0xff)
  assert.deepEqual(base64ToBytes(bytesToBase64(bytes)), bytes)
  assert.deepEqual(Array.from(base64ToBytes('aGk=\n')), [0x68, 0x69])
})

test('detectEol and scanBreaks agree on the main line ending', () => {
  for (const raw of ['a\r\nb\nc\r\n', 'a\nb\r\n', 'x\ry\rz\n', 'one', '\r\n\r\n\n\n\n', 'a\r\nb\n']) {
    for (const fallback of ['lf', 'crlf']) {
      const d = detectEol(raw, fallback)
      const s = scanBreaks(raw, fallback)
      assert.equal(s.main, d.eol, JSON.stringify(raw))
      assert.equal(s.mixed, d.mixed, JSON.stringify(raw))
    }
  }
})

test('save bodies: UTF-8 as text (BOM as U+FEFF), legacy encodings as base64; meta equality includes mixedEol', () => {
  assert.deepEqual(writeBody('héllo', { encoding: 'utf-8', bom: true }), { content: '\uFEFFhéllo', encoding: 'utf-8' })
  assert.deepEqual(writeBody('é', { encoding: 'windows-1252', bom: false }), { content: '6Q==', encoding: 'base64' })
  assert.equal(withEol('a\nb\n', 'crlf'), 'a\r\nb\r\n')
  const m = { encoding: 'utf-8', bom: false, eol: 'lf', mixedEol: true }
  assert.equal(sameMeta(m, { ...m }), true)
  assert.equal(sameMeta(m, { ...m, mixedEol: false }), false)
})

test('remoteChanged: instants, same-second size changes, missing times', () => {
  const base = { mtime: '2026-09-27T17:00:00.123456789Z', size: 10 }
  assert.equal(remoteChanged(base, { mtime: '2026-09-27T17:00:00.123Z', size: 10 }), false)
  assert.equal(remoteChanged(base, { mtime: '2026-09-27T20:00:00.123+03:00', size: 10 }), false)
  assert.equal(remoteChanged(base, { mtime: '2026-09-27T17:00:01Z', size: 10 }), true)
  assert.equal(remoteChanged(base, { mtime: '2026-09-27T17:00:00.123Z', size: 11 }), true)
  assert.equal(remoteChanged({ size: 0 }, { mtime: '2026-09-27T17:00:00Z', size: 5 }), true)
  assert.equal(remoteChanged({ size: 5 }, { size: 5 }), false)
})

test('colour swatches: literals found, look-alikes ignored', () => {
  const texts = (s) => findColors(s).map((m) => m[2])
  assert.deepEqual(texts('a { color: #fff; background: #12345678; border-color: #abcd }'), ['#fff', '#12345678', '#abcd'])
  assert.deepEqual(texts('rgb(1, 2, 3) rgba(1 2 3 / 50%) hsl(210deg 40% 20%) hsla(1,2%,3%,.5)'), ['rgb(1, 2, 3)', 'rgba(1 2 3 / 50%)', 'hsl(210deg 40% 20%)', 'hsla(1,2%,3%,.5)'])
  assert.deepEqual(texts('&#123; id#abc a#fff #12 #12345 #ggg foo-#abc'), [])
})

test('font family values cannot break out of the CSS rule', () => {
  assert.equal(safeFontFamily('"Fira Code", monospace'), '"Fira Code", monospace')
  assert.equal(safeFontFamily('x; } body { display: none } .y {'), 'x body display none .y')
  assert.equal(safeFontFamily('url(javascript:alert(1))'), 'urljavascriptalert1')
  assert.equal(safeFontFamily('"unbalanced'), 'unbalanced')
  assert.equal(safeFontFamily(''), '')
})

test('HTML export escapes content and title, numbers every line and colours tokens', () => {
  const lines = ['<script>alert(1)</script>', '', '& "q"']
  const html = renderHtml(lines, null, { title: 'a<b>.txt', theme: 'light', lineNumbers: true, tabSize: 4 })
  assert.ok(!/<script/i.test(html.replace('<!DOCTYPE html>', '')))
  assert.ok(html.includes('<title>a&lt;b&gt;.txt</title>'))
  assert.ok(html.includes('&lt;script&gt;alert(1)&lt;/script&gt;'))
  assert.equal((html.match(/<span class="l">/g) ?? []).length, 3)
  assert.ok(html.includes('&amp; &quot;q&quot;'))
  assert.equal(escapeHtml('<&>"'), '&lt;&amp;&gt;&quot;')
  // Monaco tokens: "const" is a keyword, the rest plain / string
  const tokens = [[{ offset: 0, type: 'keyword.js' }, { offset: 5, type: '' }, { offset: 10, type: 'string.js' }]]
  const colored = renderHtml(["const x = '<'"], tokens, { title: 't', theme: 'dark', lineNumbers: false, tabSize: 2 })
  const kw = ruleIndexOf('keyword')
  const str = ruleIndexOf('string')
  assert.ok(colored.includes(`<span class="l"><span class="s${kw}">const</span> x = <span class="s${str}">'&lt;'</span></span>`), colored)
  assert.ok(colored.includes(`.s${kw}{color:#c678dd}`))
})

test('token rules: most specific scope wins, like Monaco', () => {
  assert.equal(SYNTAX_RULES[ruleIndexOf('keyword.flow.js')].token, 'keyword.flow')
  assert.equal(SYNTAX_RULES[ruleIndexOf('keyword.json')].token, 'keyword.json')
  assert.equal(SYNTAX_RULES[ruleIndexOf('string.key.json')].token, 'string.key.json')
  assert.equal(SYNTAX_RULES[ruleIndexOf('comment.block.js')].token, 'comment')
  assert.equal(ruleIndexOf('source'), -1)
  assert.equal(ruleIndexOf(''), -1)
  assert.equal(tokenColor('comment.line', 'light'), '#6e7781')
  assert.equal(tokenColor('identifier.js', 'light'), '')
})

test('design-token colours convert to Monaco hex (oklch, oklab, rgb, hsl, hex, alpha)', () => {
  // the dark panel token and a white foreground
  assert.equal(toHex(parseColor('oklch(1 0 0)')), '#ffffff')
  assert.equal(toHex(parseColor('oklch(0 0 0)')), '#000000')
  const accent = parseColor('oklch(0.62 0.18 252)')
  assert.ok(accent && accent.b > accent.r && accent.b > 180, JSON.stringify(accent))
  assert.equal(toHex(parseColor('oklab(0.62795 0.22486 0.12585)')), '#ff0000')
  assert.equal(toHex(parseColor('oklch(0.62796 0.25768 29.2339)')), '#ff0000')
  assert.equal(toHex(parseColor('rgb(10 20 30 / 50%)')), '#0a141e80')
  assert.equal(toHex(parseColor('rgba(255, 0, 0, 0.25)')), '#ff000040')
  assert.equal(toHex(parseColor('hsl(120deg 100% 25%)')), '#008000')
  assert.equal(toHex(parseColor('#abc')), '#aabbcc')
  assert.equal(toHex(parseColor('color(srgb 1 0.5 0)')), '#ff8000')
  // the production CSS build emits the tokens as lab(): CSS Color 4 reference values
  assert.equal(toHex(parseColor('lab(54.29% 80.8 69.89)')), '#ff0000')
  assert.equal(toHex(parseColor('lab(100% 0 0)')), '#ffffff')
  assert.equal(toHex(parseColor('lch(54.29% 106.84 40.85)')), '#ff0000')
  const fg = parseColor('lab(90.7153% -.661671 -2.14585)')
  assert.ok(fg && fg.r > 220 && fg.b > fg.r - 2, JSON.stringify(fg))
  assert.equal(toHex(parseColor('oklch(0.62 0.18 252 / 0.3)')).length, 9)
  // named colours are left to the canvas fallback in the browser
  assert.equal(parseColor('white'), null)
  assert.equal(parseColor('var(--x)'), null)
  assert.equal(parseColor('transparent')?.a, 0)
})

test('colour literals: parse for the swatch, write back in the literal\'s style', () => {
  assert.deepEqual(literalColor('#ff0000'), { red: 1, green: 0, blue: 0, alpha: 1 })
  assert.equal(literalColor('rgb(0 0 0 / 50%)')?.alpha, 0.5)
  const green = { red: 0, green: 0.5, blue: 0, alpha: 1 }
  assert.equal(colorLiteral('#fff', green), '#008000')
  assert.equal(colorLiteral('rgb(1,2,3)', green), 'rgb(0, 128, 0)')
  assert.equal(colorLiteral('hsl(0 0% 0%)', green), 'hsl(120, 100%, 25%)')
  assert.equal(colorLiteral('rgba(1,2,3,.5)', { ...green, alpha: 0.5 }), 'rgba(0, 128, 0, 0.5)')
})

test('language detection: extensions, file names, paths, shebangs, content', () => {
  const id = (path, text = '') => detectLanguage(path, text)?.id ?? null
  assert.equal(id('/etc/nginx/sites-available/default'), 'nginx')
  assert.equal(id('/opt/app/nginx.conf'), 'nginx')
  assert.equal(id('/srv/Dockerfile'), 'dockerfile')
  assert.equal(id('/srv/Dockerfile.prod'), 'dockerfile')
  assert.equal(id('/srv/app.dockerfile'), 'dockerfile')
  assert.equal(id('/srv/docker-compose.yml'), 'yaml')
  assert.equal(id('/x/config.yaml'), 'yaml')
  assert.equal(id('/etc/php/8.2/php.ini'), 'ini')
  assert.equal(id('/etc/systemd/system/app.service'), 'ini')
  assert.equal(id('/etc/ssh/sshd_config'), 'conf')
  assert.equal(id('/home/u/.bashrc'), 'shell')
  assert.equal(id('/home/u/.env.local'), 'shell')
  assert.equal(id('/x/Cargo.toml'), 'toml')
  assert.equal(id('/x/pyproject.toml'), 'toml')
  assert.equal(id('/x/Makefile'), 'makefile')
  assert.equal(id('/x/fix.patch'), 'diff')
  assert.equal(id('/x/a.tsx'), 'typescript')
  assert.equal(id('/x/types.d.ts'), 'typescript')
  assert.equal(id('/x/package.json'), 'json')
  assert.equal(id('/x/tsconfig.json'), 'json')
  assert.equal(id('/usr/local/bin/deploy', '#!/usr/bin/env bash\necho hi'), 'shell')
  assert.equal(id('/usr/local/bin/tool', '#!/usr/bin/python3\n'), 'python')
  assert.equal(id('/usr/local/bin/srv', '#!/usr/bin/env -S node --no-warnings\n'), 'javascript')
  assert.equal(id('/x/data', '{"a": [1, 2]}'), 'json')
  assert.equal(id('/x/notes.txt', '#!/bin/sh\n'), 'shell')
  assert.equal(id('/x/readme'), null)
  assert.equal(id('/x/page', '<!DOCTYPE html>\n<html>'), 'html')
  assert.equal(id('/x/cfg', '# vim: set ft=yaml :\nkey: 1'), 'yaml')
  // names from the picker, Monaco ids and CodeMirror-era names stored in tab params
  assert.equal(findLanguage('Shell')?.id, 'shell')
  assert.equal(findLanguage('javascript')?.id, 'javascript')
  assert.equal(findLanguage('Properties files')?.id, 'conf')
  assert.equal(findLanguage('Jinja')?.id, 'twig')
  assert.equal(findLanguage('C++')?.id, 'cpp')
  assert.equal(findLanguage('nope'), null)
  const names = languageNames()
  assert.equal(names[0], 'Plain Text')
  assert.equal(new Set(names).size, names.length, 'unique picker names')
})

test('indentation: detection, conversion, visual columns', () => {
  assert.deepEqual(detectIndentation(linesOf('a\n  b\n    c\n  d')), { useTabs: false, size: 2 })
  assert.deepEqual(detectIndentation(linesOf('a\n\tb\n\t\tc')), { useTabs: true, size: 0 })
  assert.deepEqual(detectIndentation(linesOf('/**\n * doc\n */\nx\n    y\n        z')), { useTabs: false, size: 4 })
  assert.equal(detectIndentation(linesOf('flat\ntext')), null)
  assert.deepEqual(convertIndentation(linesOf('x\n    y\n\t  z'), true, 4), [
    { line: 2, length: 4, insert: '\t' },
    { line: 3, length: 3, insert: '\t  ' },
  ].slice(0, 1))
  assert.deepEqual(convertIndentation(linesOf('\tx'), false, 2), [{ line: 1, length: 1, insert: '  ' }])
  assert.equal(visualColumn('\tab', 1, 4), 5)
  assert.equal(visualColumn('a\tb', 2, 4), 5)
  assert.equal(visualColumn('abc', 3, 4), 4)
})

function rng(seed) {
  let x = seed >>> 0 || 1
  return () => {
    x ^= x << 13
    x ^= x >>> 17
    x ^= x << 5
    return (x >>> 0) / 4294967296
  }
}

test('minimal text edits: exact result, untouched lines untouched', () => {
  const cases = [
    ['a\nx', 'a'],
    ['a', 'a\nb'],
    ['', 'x'],
    ['x', ''],
    ['one\ntwo\nthree', 'one\nTWO\nthree'],
    ['a\nb\nc\nd', 'a\nc\nd'],
    ['a\nb\nc', 'x\na\nb\nc'],
    ['same', 'same'],
  ]
  for (const [a, b] of cases) assert.equal(applyEdits(a, textEdits(a, b)), b, JSON.stringify([a, b]))
  // a changed middle line is edited in place: the edit never covers the line breaks around it
  const e = textEdits('keep\nold line\nkeep', 'keep\nnew line\nkeep')
  assert.deepEqual(e, [{ offset: 5, length: 3, text: 'new' }])
  // randomized
  const words = ['alpha', 'beta', 'gamma', 'delta', '', 'x y', 'zz']
  for (let seed = 1; seed <= 200; seed++) {
    const r = rng(seed * 31337)
    const gen = () => Array.from({ length: Math.floor(r() * 12) }, () => words[Math.floor(r() * words.length)]).join('\n')
    const a = gen()
    const lines = a.split('\n')
    for (let k = 0; k < 4; k++) {
      const i = Math.floor(r() * (lines.length + 1))
      const op = r()
      if (op < 0.33) lines.splice(i, 1)
      else if (op < 0.66) lines.splice(i, 0, words[Math.floor(r() * words.length)])
      else if (i < lines.length) lines[i] += '!'
    }
    const b = lines.join('\n')
    const edits = textEdits(a, b)
    assert.equal(applyEdits(a, edits), b, `seed ${seed}`)
    for (let k = 1; k < edits.length; k++) assert.ok(edits[k].offset >= edits[k - 1].offset + edits[k - 1].length, 'ascending, non-overlapping')
    for (const ed of edits) assert.ok(ed.offset + ed.length <= a.length, 'inside the old text')
  }
})
