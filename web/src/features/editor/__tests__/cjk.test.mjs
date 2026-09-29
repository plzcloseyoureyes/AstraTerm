// Multi-byte legacy encoders (cjk.ts) against the platform's WHATWG decoders.
import assert from 'node:assert/strict'
import test from 'node:test'
import { encodeMultiByte, MULTIBYTE_ENCODINGS, UnencodableError } from '../cjk'
import { decodeBytes, encodeText, EncodeError, roundTrips } from '../codec'

const hex = (b) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')

test('known WHATWG byte sequences', () => {
  const cases = [
    ['shift_jis', 'aあｱ①', '6182a0b18740'],
    ['shift_jis', '纊', 'fa5c'], // IBM extension: 0xFA5C, never the NEC-selected 0xED40
    ['euc-jp', 'aあｱ', '61a4a28eb1'],
    ['gbk', '中€', 'd6d080'],
    ['gb18030', '中€', 'd6d0a2e3'],
    ['gb18030', '\u0080😀', '8130813094' + '39fc36'],
    ['big5', '中', 'a4a4'],
    ['big5', '═', 'f9f9'], // U+2550: the last Big5 sequence
    ['euc-kr', '한글', 'c7d1b1db'],
  ]
  for (const [enc, text, want] of cases) assert.equal(hex(encodeMultiByte(text, enc)), want, `${enc} ${text}`)
})

test('every encodable character decodes back to itself, for every encoding', () => {
  for (const enc of MULTIBYTE_ENCODINGS) {
    const dec = new TextDecoder(enc)
    let checked = 0
    // CJK ideographs, kana, hangul, symbols, full-width forms
    for (const [lo, hi] of [
      [0x3000, 0x30ff],
      [0x4e00, 0x4fff],
      [0x9f00, 0x9fa5],
      [0xac00, 0xad00],
      [0xff01, 0xff9f],
      [0x2460, 0x24ff],
      [0x00a0, 0x00ff],
    ]) {
      for (let cp = lo; cp <= hi; cp++) {
        const ch = String.fromCodePoint(cp)
        let bytes
        try {
          bytes = encodeMultiByte(ch, enc)
        } catch (e) {
          assert.ok(e instanceof UnencodableError)
          continue
        }
        assert.equal(dec.decode(bytes), ch, `${enc} U+${cp.toString(16)} → ${hex(bytes)}`)
        checked++
      }
    }
    assert.ok(checked > 500, `${enc}: only ${checked} characters encodable`)
  }
})

test('GB18030 writes every BMP and supplementary character (four-byte sequences)', () => {
  const dec = new TextDecoder('gb18030')
  for (const cp of [0x80, 0xa5, 0x0452, 0x2010, 0x3400, 0x4dae, 0xe000, 0xfe10, 0xffe6, 0x10000, 0x1f600, 0x10ffff]) {
    const ch = String.fromCodePoint(cp)
    assert.equal(dec.decode(encodeMultiByte(ch, 'gb18030')), ch, `U+${cp.toString(16)}`)
  }
})

test('characters outside the code page are reported with their index', () => {
  assert.throws(() => encodeMultiByte('ab😀', 'shift_jis'), (e) => e instanceof UnencodableError && e.index === 2)
  assert.throws(() => encodeMultiByte('한', 'shift_jis'), UnencodableError)
  assert.throws(() => encodeText('x한', 'euc-jp', false), (e) => e instanceof EncodeError && e.index === 1)
})

test('codec: multi-byte files round-trip byte for byte; foreign sequences are detected', () => {
  const sjis = new Uint8Array([0x82, 0xa0, 0x0d, 0x0a, 0x93, 0xfa, 0x96, 0x7b, 0x8c, 0xea, 0x0a, 0xb1])
  const d = decodeBytes(sjis, 'shift_jis')
  assert.equal(d.text, 'あ\r\n日本語\nｱ')
  assert.deepEqual(Array.from(encodeText(d.text, 'shift_jis', false)), Array.from(sjis))
  assert.equal(roundTrips(sjis, d.text, 'shift_jis', false), true)
  // NEC-selected IBM extension bytes decode fine but are written the WHATWG way (0xFA..): not an exact round trip
  const nec = new Uint8Array([0xed, 0x40])
  assert.equal(roundTrips(nec, decodeBytes(nec, 'shift_jis').text, 'shift_jis', false), false)
})
