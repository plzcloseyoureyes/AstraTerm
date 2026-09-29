#!/usr/bin/env node
/*
 * Flash audit (docs/UX.md "Loading states"): drives the built binary in headless Chrome and MEASURES flashing.
 *
 *   make build && node scripts/flash-audit.mjs [--latency 250] [--strict] [--keep] [--bin bin/nexterm]
 *
 * It starts a throwaway NexTerm (temporary HOME/USERPROFILE and data dir, random loopback port, reached as
 * http://flash-audit.localhost:<port>), a headless Chrome with its own temporary profile (downloads denied), and runs
 * the scenarios below while recording every painted frame (Page.startScreencast) plus a DOM probe:
 *
 *   visual flashes   a region (≥ 2 % of the screen, on a 48×30 luminance grid) changes and changes BACK within 400 ms
 *                    (A → B → A: a spinner/skeleton/blank that came and went, a theme or content blink)
 *   dom flashes      a loading indicator ([role=status], [role=progressbar], .animate-spin, [data-busy-indicator])
 *                    visible for less than MIN_VISIBLE (500) ms (the primitives keep them ≥ 600 ms)
 *   oscillation      anywhere on screen (6 px cells, full resolution): a small element that keeps going back and forth
 *                    — a pulse / breathe (gradual) or a blink (toggling) — see oscillations(); only the spinner's rotation
 *                    is exempt (a text caret is not: every editable surface must show a steady one, see the caret
 *                    scenarios)
 *   small flashes    in the folder-navigation scenarios (inside the file browser): a bar / spinner / badge / highlight
 *                    that appears and disappears within 1 s (see roiFlashes), an indicator shown < 750 ms, or more
 *                    indicator cycles than navigation gestures
 *
 * Scenarios: cold load (setup screen) · launch-link load into the shell · open a local terminal · reload with the
 * terminal open · open Local files / System information / Settings · switch tabs · switch Settings sections · close a
 * tab and reopen it / close a terminal and Undo (functional checks: both return to their place) · the phone "all tabs"
 * sheet · reload again · group the session tree by tag and back · a steady caret in a text field, the terminal, the
 * script editor (CodeMirror) and the file editor (Monaco), 3.5 s each · pop a tab out and dock it back into an empty grid
 * (functional check: the tab must be visible with a real size). Every recording also flags a frame that shows another
 * tab's content under the active tab ("previous tab content"). Then (base run only,
 * --nav-latencies 0,150,350,700,1500): folder navigation in the files tab and — with an SSH target (--ssh, default: the
 * Docker lab's ssh1 when it is up) — connecting SSH (0 / 350 / 1500 ms), reconnecting, and navigation / follow-cd /
 * idle background refresh in the SFTP side panel; slow navigations must show exactly one calm indicator.
 * --latency N adds N ms to every request (CDP network emulation) to expose indicators that fast loopback hides.
 * --motion reduce|no-preference overrides prefers-reduced-motion (default: the OS setting, which headless Chrome uses).
 * Exit code: 0; with --strict, 1 when any flash (or a failed check) was found. Requires Node ≥ 22 and Chrome
 * (CHROME=/path overrides the default location).
 */
import { spawn } from 'node:child_process'
import { existsSync, mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { inflateSync } from 'node:zlib'
import { randomBytes } from 'node:crypto'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const { values: opt } = parseArgs({
  options: {
    bin: { type: 'string', default: path.join(ROOT, 'bin', process.platform === 'win32' ? 'nexterm.exe' : 'nexterm') },
    latency: { type: 'string', default: '0' },
    strict: { type: 'boolean', default: false },
    keep: { type: 'boolean', default: false },
    width: { type: 'string', default: '1400' },
    height: { type: 'string', default: '900' },
    // Folder-navigation scenarios (files tab + SFTP side panel), each run under these request latencies (ms). They run
    // only in the base run without --latency (they set their own). "" skips them.
    'nav-latencies': { type: 'string', default: '0,150,350,700,1500' },
    // SSH target for the SFTP side panel: user:password@host:port, "auto" (the Docker lab's ssh1 when it is up —
    // scripts/testenv) or "off".
    ssh: { type: 'string', default: 'auto' },
    // Skip the boot / tab scenarios (quicker iteration on the navigation ones).
    'nav-only': { type: 'boolean', default: false },
    // prefers-reduced-motion for the run: system (the OS setting headless Chrome inherits), reduce or no-preference.
    motion: { type: 'string', default: 'system' },
  },
})
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const CHROME =
  process.env.CHROME ||
  {
    darwin: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    win32: 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  }[process.platform] ||
  'google-chrome'

// ---------------------------------------------------------------------------------------------------------------------
// throwaway server + browser
// ---------------------------------------------------------------------------------------------------------------------

const work = mkdtempSync(path.join(tmpdir(), 'nexterm-flash-audit-'))
const home = path.join(work, 'home')
mkdirSync(home, { recursive: true, mode: 0o700 })
const cleanups = []
process.on('exit', () => {
  for (const c of cleanups.reverse()) {
    try {
      c()
    } catch {
      /* ignore */
    }
  }
})
for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => process.exit(130))

async function startServer() {
  if (!existsSync(opt.bin)) throw new Error(`${opt.bin} not found — run make build first`)
  const env = { ...process.env, HOME: home, USERPROFILE: home, NEXTERM_NO_OPEN: '1' }
  const args = ['serve', '--listen', '127.0.0.1:0', '--data-dir', path.join(work, 'data'), '--no-open', '--guacd', 'off']
  const proc = spawn(opt.bin, args, { env, stdio: ['ignore', 'pipe', 'ignore'] })
  cleanups.push(() => proc.kill('SIGTERM'))
  let out = ''
  const url = await new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error('server did not start')), 20000)
    proc.stdout.on('data', (d) => {
      out += d
      const m = out.match(/URL:\s+(http:\/\/127\.0\.0\.1:(\d+)\/\S*)/)
      if (m) {
        clearTimeout(t)
        resolve(m[1])
      }
    })
  })
  const u = new URL(url)
  return { base: `http://flash-audit.localhost:${u.port}`, launch: u.searchParams.get('launch') }
}

async function startChrome() {
  const profile = path.join(work, 'chrome')
  const proc = spawn(CHROME, ['--headless=new', '--remote-debugging-port=0', `--user-data-dir=${profile}`,
    `--window-size=${opt.width},${opt.height}`, '--force-device-scale-factor=1', '--no-first-run', '--no-default-browser-check', 'about:blank'])
  cleanups.push(() => proc.kill('SIGKILL'))
  const wsUrl = await new Promise((resolve, reject) => {
    let buf = ''
    const t = setTimeout(() => reject(new Error('chrome did not start: ' + buf)), 20000)
    proc.stderr.on('data', (d) => {
      buf += d
      const m = buf.match(/DevTools listening on (ws:\/\/\S+)/)
      if (m) {
        clearTimeout(t)
        resolve(m[1])
      }
    })
  })
  const port = new URL(wsUrl).port
  const page = (await (await fetch(`http://127.0.0.1:${port}/json/list`)).json()).find((t) => t.type === 'page')
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((r) => ws.addEventListener('open', r, { once: true }))
  let id = 0
  const pending = new Map()
  const listeners = new Set()
  ws.addEventListener('message', (e) => {
    const msg = JSON.parse(e.data)
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg)
      pending.delete(msg.id)
    } else if (msg.method) for (const l of listeners) l(msg)
  })
  /** When input was dispatched (epoch ms): the oscillation detector ignores what the user's own input changes. */
  const inputs = []
  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      if (method.startsWith('Input.')) inputs.push(Date.now())
      const i = ++id
      pending.set(i, (m) => (m.error ? reject(new Error(`${method}: ${m.error.message}`)) : resolve(m.result)))
      ws.send(JSON.stringify({ id: i, method, params }))
    })
  const evaluate = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text)
    return r.result.value
  }
  const loaded = () =>
    new Promise((r) => {
      const l = (m) => {
        if (m.method === 'Page.loadEventFired') {
          listeners.delete(l)
          r()
        }
      }
      listeners.add(l)
    })
  const key = async (k, code, keyCode, modifiers = 0) => {
    await send('Input.dispatchKeyEvent', { type: 'rawKeyDown', key: k, code, windowsVirtualKeyCode: keyCode, modifiers })
    await send('Input.dispatchKeyEvent', { type: 'keyUp', key: k, code, windowsVirtualKeyCode: keyCode, modifiers })
  }
  await send('Page.enable')
  await send('Runtime.enable')
  await send('Network.enable')
  await send('Browser.setDownloadBehavior', { behavior: 'deny' }).catch(() => {})
  return { send, evaluate, loaded, key, listeners, inputs }
}

// ---------------------------------------------------------------------------------------------------------------------
// frames → luminance grids → flash events
// ---------------------------------------------------------------------------------------------------------------------

const GW = 48
const GH = 30

/** Minimal PNG decoder (8-bit RGB / RGBA, as Chrome's screencast produces) → {w, h, bpp, stride, px}. */
function decodePng(buf) {
  let pos = 8
  let w = 0
  let h = 0
  let ct = 0
  const idat = []
  while (pos < buf.length) {
    const len = buf.readUInt32BE(pos)
    const type = buf.toString('latin1', pos + 4, pos + 8)
    const data = buf.subarray(pos + 8, pos + 8 + len)
    if (type === 'IHDR') {
      w = data.readUInt32BE(0)
      h = data.readUInt32BE(4)
      ct = data[9]
      if (data[8] !== 8 || (ct !== 2 && ct !== 6)) throw new Error('unsupported PNG')
    } else if (type === 'IDAT') idat.push(data)
    pos += 12 + len
  }
  const bpp = ct === 6 ? 4 : 3
  const raw = inflateSync(Buffer.concat(idat))
  const stride = w * bpp
  const px = Buffer.alloc(stride * h)
  for (let y = 0; y < h; y++) {
    const f = raw[y * (stride + 1)]
    const src = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1))
    const out = px.subarray(y * stride, (y + 1) * stride)
    const prev = y ? px.subarray((y - 1) * stride, y * stride) : null
    for (let x = 0; x < stride; x++) {
      const a = x >= bpp ? out[x - bpp] : 0
      const b = prev ? prev[x] : 0
      const c = prev && x >= bpp ? prev[x - bpp] : 0
      let v = src[x]
      if (f === 1) v += a
      else if (f === 2) v += b
      else if (f === 3) v += (a + b) >> 1
      else if (f === 4) {
        const p = a + b - c
        const pa = Math.abs(p - a)
        const pb = Math.abs(p - b)
        const pc = Math.abs(p - c)
        v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c
      }
      out[x] = v & 255
    }
  }
  return { w, h, bpp, stride, px }
}

/** Luminance grid GW×GH of a whole frame. */
function pngGrid(buf) {
  return gridOf(decodePng(buf))
}

function gridOf({ w, h, bpp, stride, px }) {
  const grid = new Float32Array(GW * GH)
  const cnt = new Uint32Array(GW * GH)
  for (let y = 0; y < h; y += 2) {
    const gy = Math.min(GH - 1, Math.floor((y / h) * GH))
    for (let x = 0; x < w; x += 2) {
      const i = y * stride + x * bpp
      const g = gy * GW + Math.min(GW - 1, Math.floor((x / w) * GW))
      grid[g] += 0.2126 * px[i] + 0.7152 * px[i + 1] + 0.0722 * px[i + 2]
      cnt[g]++
    }
  }
  for (let i = 0; i < grid.length; i++) grid[i] /= cnt[i] || 1
  return grid
}

/**
 * A → B → A within 400 ms on ≥ 2 % of the cells (luminance delta ≥ 12). Returns the events [{at, ms, cells}].
 * `since` (epoch ms) ignores base frames painted before it: a reload tears the old page down, so "old page → splash →
 * new page" is a navigation, not a flash (flashes inside the new page's boot are still found).
 */
function visualFlashes(unordered, since = 0) {
  // Screencast frames can arrive slightly out of order: analyse them in paint order.
  const frames = [...unordered].sort((a, b) => a.t - b.t)
  const T = 12
  const MIN_CELLS = Math.ceil(GW * GH * 0.02)
  const events = []
  for (let i = 1; i < frames.length; i++) {
    const base = frames[i - 1]
    if (base.t < since) continue
    const changed = []
    for (let c = 0; c < GW * GH; c++) if (Math.abs(frames[i].grid[c] - base.grid[c]) >= T) changed.push(c)
    if (changed.length < MIN_CELLS) continue
    for (let j = i + 1; j < frames.length && frames[j].t - frames[i].t < 400; j++) {
      const back = changed.filter((c) => Math.abs(frames[j].grid[c] - base.grid[c]) < T / 3).length
      if (back >= MIN_CELLS && back >= changed.length * 0.6) {
        events.push({ at: Math.round(frames[i].t), ms: Math.round(frames[j].t - frames[i].t), cells: back, frames: [frames[i - 1], frames[i], frames[j]] })
        break
      }
    }
  }
  return events
}

/**
 * Small-region flashes inside a region of interest (a file browser): the whole-screen grid above averages a 2 px
 * progress bar or a 12 px spinner away, so ROI frames are cut into CELL×CELL px cells. A change of ≥ ROI_MIN_CELLS
 * cells that goes BACK within ROI_WINDOW ms is a flash — a bar / spinner / badge that appeared and disappeared (or an
 * animated stripe sweeping through). The scenarios keep the pointer still and space their steps so legitimate
 * one-way changes (a new listing, a moved selection) never revert inside the window.
 */
const CELL = 6
const ROI_WINDOW = 1000
const ROI_MIN_CELLS = 3

function roiGrid(img, roi) {
  const x0 = Math.max(0, Math.floor(roi.x * roi.scale))
  const y0 = Math.max(0, Math.floor(roi.y * roi.scale))
  const x1 = Math.min(img.w, Math.floor((roi.x + roi.w) * roi.scale))
  const y1 = Math.min(img.h, Math.floor((roi.y + roi.h) * roi.scale))
  const cw = Math.max(1, Math.floor((x1 - x0) / CELL))
  const ch = Math.max(1, Math.floor((y1 - y0) / CELL))
  const grid = new Float32Array(cw * ch)
  // Cell (0,0) in CSS px, for mapping events back onto the page (allowed indicators).
  const origin = { x: x0 / roi.scale, y: y0 / roi.scale, scale: roi.scale }
  for (let cy = 0; cy < ch; cy++) {
    for (let cx = 0; cx < cw; cx++) {
      let sum = 0
      for (let y = y0 + cy * CELL; y < y0 + (cy + 1) * CELL; y++) {
        const row = y * img.stride
        for (let x = x0 + cx * CELL; x < x0 + (cx + 1) * CELL; x++) {
          const i = row + x * img.bpp
          sum += 0.2126 * img.px[i] + 0.7152 * img.px[i + 1] + 0.0722 * img.px[i + 2]
        }
      }
      grid[cy * cw + cx] = sum / (CELL * CELL)
    }
  }
  return { grid, cw, ch, origin }
}

/** `allowed`: page rects (CSS px) of indicators that behaved (shown ≥ NAV_MIN_VISIBLE): their own appearance is fine. */
function roiFlashes(unordered, allowed = []) {
  // Screencast frames can arrive slightly out of order: analyse them in paint order.
  const frames = [...unordered].sort((a, b) => a.t - b.t)
  const T = 12
  const events = []
  let skipUntil = 0
  for (let i = 1; i < frames.length; i++) {
    const base = frames[i - 1]
    if (frames[i].t < skipUntil || !base.roi || !frames[i].roi || base.roi.grid.length !== frames[i].roi.grid.length) continue
    const changed = []
    for (let c = 0; c < base.roi.grid.length; c++) if (Math.abs(frames[i].roi.grid[c] - base.roi.grid[c]) >= T) changed.push(c)
    if (changed.length < ROI_MIN_CELLS) continue
    for (let j = i + 1; j < frames.length && frames[j].t - frames[i].t < ROI_WINDOW; j++) {
      if (!frames[j].roi || frames[j].roi.grid.length !== base.roi.grid.length) break
      const back = changed.filter((c) => Math.abs(frames[j].roi.grid[c] - base.roi.grid[c]) < T / 3)
      if (back.length >= ROI_MIN_CELLS && back.length >= changed.length * 0.6) {
        const cw = base.roi.cw
        const ys = back.map((c) => Math.floor(c / cw))
        const xs = back.map((c) => c % cw)
        const o = base.roi.origin
        const page = {
          x: o.x + (Math.min(...xs) * CELL) / o.scale,
          y: o.y + (Math.min(...ys) * CELL) / o.scale,
          w: ((Math.max(...xs) - Math.min(...xs) + 1) * CELL) / o.scale,
          h: ((Math.max(...ys) - Math.min(...ys) + 1) * CELL) / o.scale,
        }
        const pad = CELL
        const covered = allowed.some((a) => page.x >= a.x - pad && page.y >= a.y - pad && page.x + page.w <= a.x + a.w + pad && page.y + page.h <= a.y + a.h + pad)
        skipUntil = frames[j].t // one event per blink, not one per frame of it
        if (covered) break
        events.push({
          at: Math.round(frames[i].t),
          ms: Math.round(frames[j].t - frames[i].t),
          cells: back.length,
          box: `${Math.min(...xs) * CELL},${Math.min(...ys) * CELL} ${(Math.max(...xs) - Math.min(...xs) + 1) * CELL}×${(Math.max(...ys) - Math.min(...ys) + 1) * CELL}px`,
          frames: [base, frames[i], frames[j]],
        })
        break
      }
    }
  }
  return events
}

/**
 * Oscillation (docs/UX.md: nothing may pulse, breathe or blink): a cell whose luminance keeps swinging between the
 * same two levels on its own. Per cell, the turning points of its luminance (swings of ≥ 12) are collected; a run of
 * OSC_EXTREMES turning points where every one comes back to the level of the one two before (peak ≈ peak, trough ≈
 * trough) and follows the previous one within OSC_HALF_PERIOD ms is an oscillation — a gradual pulse / breathe as
 * well as an on / off blink. Data that updates every couple of seconds (monitor numbers) is slower and rarely returns
 * to the same two levels; frames right after the user's own input and the spinner's rotation are excluded (a blinking
 * text caret is an oscillation like any other). Works on `roi` grids (a region or the whole viewport, see roiGrid). One event per
 * scenario, with the bounding box of the oscillating cells.
 */
const OSC_EXTREMES = 4
const OSC_HALF_PERIOD = 1600

/** Frames painted while nothing was typed or clicked for INPUT_QUIET ms: changes there happen on their own. */
const INPUT_QUIET = 700

function oscillations(unordered, allowed = [], inputs = []) {
  const frames = [...unordered]
    .filter((f) => f.roi && !inputs.some((t) => f.t >= t && f.t - t < INPUT_QUIET))
    .sort((a, b) => a.t - b.t)
  if (frames.length < 4) return []
  const T = 12
  const n = frames[0].roi.grid.length
  const { cw, origin: o } = frames[0].roi
  const hits = []
  for (let c = 0; c < n; c++) {
    let ext = frames[0].roi.grid[c] // last turning point
    let dir = 0 // current direction of travel
    let cand = ext // the furthest value in `dir` so far
    let candT = frames[0].t
    const turns = [] // {v, t}
    let run = 0
    for (const f of frames) {
      if (f.roi.grid.length !== n) break
      const v = f.roi.grid[c]
      if (dir === 0) {
        if (Math.abs(v - ext) >= T) {
          dir = Math.sign(v - ext)
          cand = v
          candT = f.t
        }
        continue
      }
      if ((v - cand) * dir > 0) {
        cand = v
        candT = f.t
        continue
      }
      if (Math.abs(v - cand) < T) continue
      // A turning point at `cand`.
      turns.push({ v: cand, t: candT })
      const k = turns.length
      const back = k >= 3 && Math.abs(turns[k - 1].v - turns[k - 3].v) < T / 2
      const quick = k >= 2 && turns[k - 1].t - turns[k - 2].t <= OSC_HALF_PERIOD
      run = back && quick ? Math.max(run, 2) + 1 : quick ? 2 : 1
      if (run >= OSC_EXTREMES) break
      ext = cand
      dir = -dir
      cand = v
      candT = f.t
    }
    if (run < OSC_EXTREMES) continue
    const x = o.x + ((c % cw) * CELL) / o.scale
    const y = o.y + (Math.floor(c / cw) * CELL) / o.scale
    const size = CELL / o.scale
    if (allowed.some((a) => x >= a.x - size && y >= a.y - size && x + size <= a.x + a.w + size && y + size <= a.y + a.h + size)) continue
    hits.push({ x, y })
  }
  if (hits.length < 2) return []
  const xs = hits.map((h) => h.x)
  const ys = hits.map((h) => h.y)
  return [{ cells: hits.length, oscillating: true, box: `${Math.round(Math.min(...xs))},${Math.round(Math.min(...ys))} ${Math.round(Math.max(...xs) - Math.min(...xs) + CELL)}×${Math.round(Math.max(...ys) - Math.min(...ys) + CELL)}px (page)`, ms: 0, frames: [] }]
}

const DOM_PROBE = `(() => {
  if (window.__flashAudit) return
  const seen = new Map(); const done = []
  const sel = '[role=status], [role=progressbar], .animate-spin, .animate-indeterminate, [data-busy-indicator]'
  const tick = () => {
    const now = performance.now(); const live = new Set()
    for (const el of document.querySelectorAll(sel)) {
      const r = el.getBoundingClientRect(); if (!r.width || !r.height) continue
      live.add(el); if (!seen.has(el)) seen.set(el, { t: now, x: r.x, y: r.y, w: r.width, h: r.height, what: (el.getAttribute('aria-label') || el.tagName) + ' in ' + (el.closest('[data-tab-kind]')?.dataset.tabKind || el.parentElement?.className?.toString().slice(0, 40) || '?') })
    }
    for (const [el, v] of seen) if (!live.has(el)) { done.push({ what: v.what, ms: Math.round(now - v.t), x: v.x, y: v.y, w: v.w, h: v.h }); seen.delete(el) }
    // Where motion is legitimate: the spinner's rotation (nothing else — not a text caret).
    const moving = [...document.querySelectorAll('.animate-spin')]
    for (const el of moving) {
      const r = el.getBoundingClientRect(); if (!r.width || !r.height) continue
      const k = [r.x, r.y, r.width, r.height].map(Math.round).join(',')
      if (!motion.has(k)) motion.set(k, { x: r.x, y: r.y, w: r.width, h: r.height })
    }
    // Stale tab content: the active group's active tab is not the panel on screen there (the previous tab's content,
    // or another's, still showing while the new tab is marked active).
    const act = document.querySelector('.dv-active-group .dv-tab.dv-active-tab [data-tab-id]')
    const area = document.querySelector('.dv-active-group .dv-content-container')
    let staleNow = null
    if (act && area) {
      const g = area.getBoundingClientRect()
      for (const p of document.querySelectorAll('.nx-panel[data-tab-id]')) {
        if (p.dataset.tabId === act.dataset.tabId) continue
        const r = p.getBoundingClientRect()
        if (r.width < 20 || r.height < 20 || !p.checkVisibility({ visibilityProperty: true })) continue
        const ix = Math.max(0, Math.min(r.right, g.right) - Math.max(r.left, g.left))
        const iy = Math.max(0, Math.min(r.bottom, g.bottom) - Math.max(r.top, g.top))
        if (ix * iy > 0.5 * g.width * g.height) staleNow = p.dataset.tabKind + ' under the active tab "' + act.textContent.trim().slice(0, 30) + '"'
      }
    }
    if (staleNow && !stale) stale = { t: now, what: staleNow }
    if (!staleNow && stale) { staleDone.push({ what: 'previous tab content (' + stale.what + ')', ms: Math.round(now - stale.t) || 1 }); stale = null }
    requestAnimationFrame(tick)
  }
  const motion = new Map()
  let stale = null; const staleDone = []
  requestAnimationFrame(tick)
  window.__flashAuditStale = () => staleDone.splice(0)
  window.__flashAudit = () => done.splice(0)
  window.__flashAuditMotion = () => { const out = [...motion.values()]; motion.clear(); return out }
})()`

// ---------------------------------------------------------------------------------------------------------------------
// folder navigation (files tab, SFTP side panel)
// ---------------------------------------------------------------------------------------------------------------------

/** Any loading indicator visible for less than this is a blink (the primitives keep them ≥ 600 ms; ~100 ms of rAF sampling slack). */
const MIN_VISIBLE = 500
/** A loading indicator inside a file browser visible for less than this is a blink (navigation-like: ≥ 800 ms). */
const NAV_MIN_VISIBLE = 750
const NAV_TREE = ['nav/alpha/one', 'nav/alpha/two', 'nav/beta/one', 'nav/gamma']

async function sshTarget() {
  if (opt.ssh === 'off') return null
  if (opt.ssh !== 'auto') {
    const m = opt.ssh.match(/^([^:@]+):([^@]*)@([^:]+):(\d+)$/)
    if (!m) throw new Error('--ssh must be user:password@host:port, auto or off')
    return { username: m[1], password: m[2], host: m[3], port: Number(m[4]) }
  }
  // The Docker lab (scripts/testenv): ssh1 on 127.0.0.1:22022, throwaway credentials test / test.
  const { connect } = await import('node:net')
  const up = await new Promise((resolve) => {
    const sock = connect({ host: '127.0.0.1', port: 22022 })
    const done = (ok) => {
      sock.destroy()
      resolve(ok)
    }
    sock.setTimeout(700, () => done(false))
    sock.once('connect', () => done(true))
    sock.once('error', () => done(false))
  })
  return up ? { username: 'test', password: 'test', host: '127.0.0.1', port: 22022 } : null
}

/**
 * Navigation scenarios. Every step waits until the browser shows the target folder (data-path, no data-pending) and
 * then keeps still for 1.2 s, so the only thing that can "change back" inside the ROI window is an indicator (or
 * content) that blinked. Double-clicks select first and wait (the selection highlight is not part of the navigation).
 * Allowed: no indicator at all for fast loads; for slow ones one calm indicator per navigation gesture (a burst of
 * rapid clicks counts once), visible ≥ NAV_MIN_VISIBLE ms; nothing for background refreshes or followed cd's
 * that are fast.
 */
async function runNavigation({ b, checks, waitFor, palette, record, recordNav }) {
  const latencies = opt['nav-latencies'].split(',').map((x) => Number(x.trim())).filter((x) => x >= 0)
  // One never-visited folder per latency (nothing can have cached or prefetched it): the slow path is exercised too.
  const tree = [...NAV_TREE, ...latencies.flatMap((ms) => [`nav/slow-${ms}`, `nav/slow-cd-${ms}`])]
  for (const d of tree) mkdirSync(path.join(home, d), { recursive: true })
  const setLatency = (ms) => b.send('Network.emulateNetworkConditions', { offline: false, latency: ms, downloadThroughput: -1, uploadThroughput: -1 })
  const pick = (sel) => `[...document.querySelectorAll(${JSON.stringify(sel)})].find((x) => x.getBoundingClientRect().width > 0 && x.checkVisibility({ visibilityProperty: true }))`
  const mouse = (type, x, y, clickCount = 1) => b.send('Input.dispatchMouseEvent', { type, x, y, button: type === 'mouseMoved' ? 'none' : 'left', clickCount })
  const point = async (sel, inner, text) => {
    const p = await b.evaluate(`(() => { const root = ${pick(sel)}; if (!root) return null; const el = [...root.querySelectorAll(${JSON.stringify(inner)})].find((e) => e.textContent.trim() === ${JSON.stringify(text)}); if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.x + Math.min(r.width / 2, 40), y: r.y + r.height / 2 } })()`)
    if (!p) {
      const seen = await b.evaluate(`(() => { const root = ${pick(sel)}; return root ? root.dataset.path + ': ' + [...root.querySelectorAll(${JSON.stringify(inner)})].map((e) => e.textContent.trim()).join(', ') : 'no view' })()`)
      throw new Error(`"${text}" not found (${seen})`)
    }
    return p
  }
  const settle = async (sel, suffix) => {
    const ok = await waitFor(`(() => { const e = ${pick(sel)}; return !!e && !e.dataset.pending && (e.dataset.path || '').endsWith(${JSON.stringify(suffix)}) })()`, 20000)
    if (!ok) throw new Error(`did not reach …${suffix}`)
    await sleep(1200)
    if (process.env.FLASH_AUDIT_DEBUG) console.log('   at', await b.evaluate(`(() => { const e = ${pick(sel)}; return e.dataset.path + ' pending=' + (e.dataset.pending || '') + ' rows=' + [...e.querySelectorAll('[role=row][data-index] [role=gridcell]:first-child')].map((x) => x.textContent.trim()).join(',') })()`))
  }
  const openRow = async (sel, name, suffix) => {
    const p = await point(sel, '[role=row][data-index] [role=gridcell]:first-child', name)
    await mouse('mouseMoved', p.x, p.y)
    await sleep(150)
    await mouse('mousePressed', p.x, p.y, 1)
    await mouse('mouseReleased', p.x, p.y, 1)
    await sleep(ROI_WINDOW + 100) // the selection highlight (and "1 selected") settles before the navigation starts
    await mouse('mousePressed', p.x, p.y, 2)
    await mouse('mouseReleased', p.x, p.y, 2)
    if (suffix) await settle(sel, suffix)
  }
  const crumb = async (sel, name, suffix, dwell = 500) => {
    const p = await point(sel, '[aria-label=Breadcrumbs] button', name)
    await mouse('mouseMoved', p.x, p.y)
    await sleep(dwell)
    await mouse('mousePressed', p.x, p.y, 1)
    await mouse('mouseReleased', p.x, p.y, 1)
    if (suffix) await settle(sel, suffix)
  }
  const up = async (sel, suffix) => {
    await b.key('Backspace', 'Backspace', 8)
    await settle(sel, suffix)
  }
  /** Type a path into the location bar (click the current folder's crumb to edit it). */
  const typePath = async (sel, text, suffix) => {
    const last = await b.evaluate(`(() => { const r = ${pick(sel)}.querySelector('[aria-label=Breadcrumbs] [aria-current=location]').getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 } })()`)
    await mouse('mouseMoved', last.x, last.y)
    await mouse('mousePressed', last.x, last.y, 1)
    await mouse('mouseReleased', last.x, last.y, 1)
    await sleep(ROI_WINDOW + 300) // the edit field and its suggestions are the user's own mode switch, not a blink
    await b.send('Input.insertText', { text })
    await b.key('Enter', 'Enter', 13)
    await settle(sel, suffix)
  }
  /**
   * 7 gestures: into a folder, deeper, Up, breadcrumb, into another, a burst of two breadcrumb clicks 80 ms apart
   * (home, then /: one gesture), a typed path back; then a typed path to a folder nothing has cached (the slow path:
   * with enough latency it must show exactly one calm indicator) and back. The burst ends on a listing unlike the one
   * it started from, so the intermediate folder the user asked for is not mistaken for a blink.
   */
  const walk = async (sel, homeName, ms) => {
    await openRow(sel, 'alpha', '/nav/alpha')
    await openRow(sel, 'one', '/nav/alpha/one')
    await up(sel, '/nav/alpha')
    await crumb(sel, 'nav', '/nav')
    await openRow(sel, 'beta', '/nav/beta')
    await crumb(sel, homeName)
    await sleep(80)
    await crumb(sel, '/', '/', 0)
    await typePath(sel, '~/nav', '/nav')
    await typePath(sel, `~/nav/slow-${ms}`, `/nav/slow-${ms}`)
    await typePath(sel, '~/nav', '/nav')
  }
  /** Slow navigations (≥ 1.5 s here) must show their indicator: the calm path is tested, not just the quiet one. */
  const expectIndicator = (name, ms, n) => {
    if (ms >= 1500) checks.push({ name: `${name}: the slow navigation showed its indicator (${n})`, ok: n >= 1 })
  }

  // --- files tab (Local files, the temporary HOME) ---
  const TAB = '[data-file-browser=tab]'
  const homeName = path.basename(home)
  // Commands mode (">"): after the earlier scenarios, recent commands / tabs could outrank the plain text.
  await b.evaluate(`document.activeElement?.blur()`)
  await palette('>Local Files')
  const opened = await waitFor(`!!${pick(TAB)}?.dataset.path`, 15000)
  if (!opened) console.log('   files tab not visible; tabs:', await b.evaluate(`[...document.querySelectorAll('.dv-tab')].map((t) => t.textContent.trim() + (t.classList.contains('dv-active-tab') ? '*' : '')).join(' | ') + ' views: ' + [...document.querySelectorAll('[data-file-browser]')].map((e) => e.dataset.fileBrowser + ' ' + Math.round(e.getBoundingClientRect().width)).join(', ')`))
  checks.push({ name: 'files tab opened', ok: opened })
  await openRow(TAB, 'nav', '/nav').catch((err) => checks.push({ name: `files tab: open ~/nav (${err.message})`, ok: false }))
  for (const ms of latencies) {
    await setLatency(ms)
    const n = await recordNav(`files tab: navigate folders (latency ${ms} ms)`, TAB, 9, () => walk(TAB, homeName, ms))
    expectIndicator('files tab', ms, n)
  }
  await setLatency(0)

  // --- SFTP side panel of an SSH terminal ---
  const target = await sshTarget()
  if (!target) {
    console.log('skip  SFTP side panel scenarios (no SSH target: start scripts/testenv or pass --ssh user:pass@host:port)')
    return
  }
  const conn = await b.evaluate(`fetch('/api/connections',{method:'POST',headers:{'X-NexTerm':'1','Content-Type':'application/json'},body:JSON.stringify(${JSON.stringify({
    name: 'flash-audit-ssh',
    protocol: 'ssh',
    host: target.host,
    port: target.port,
    username: target.username,
    authMethod: 'password',
    secrets: { password: target.password },
  })})}).then((r) => r.json())`)
  if (!conn?.id) {
    checks.push({ name: 'SFTP panel: create the SSH connection', ok: false })
    return
  }
  // Connecting (whole screen: tab dot, status bar, session tree, terminal overlay, SFTP panel): nothing may blink or
  // pulse while it connects, whatever the latency; then a reconnect of the live session.
  // Like a person: the prompt is read (≥ 700 ms on screen) before "Accept once" — a dialog closed by a script a few ms
  // after it opened would be a blink the user can never produce.
  const acceptHostKey = `(() => { const btn = [...document.querySelectorAll('button')].find((x) => /^Accept once$/.test(x.textContent.trim())); if (!btn) return false; window.__hostKeySeen ??= performance.now(); if (performance.now() - window.__hostKeySeen < 700) return false; delete window.__hostKeySeen; btn.click(); return true })()`
  for (const ms of [0, 350, 1500].filter((x) => latencies.includes(x) || x === 0)) {
    await setLatency(ms)
    await record(`connect SSH (latency ${ms} ms)`, async () => {
      await palette('flash-audit-ssh')
      await waitFor(acceptHostKey, 15000)
      await waitFor(`!!document.querySelector('.xterm-screen')`, 15000)
    }, 4000)
  }
  await setLatency(0)
  for (const ms of [0, 1500]) {
    await setLatency(ms)
    await record(`reconnect SSH (latency ${ms} ms)`, async () => {
      await palette('>Reconnect Session')
      await waitFor(acceptHostKey, 8000)
    }, 4000)
  }
  await setLatency(0)
  const PANEL = '[data-file-browser=panel]'
  checks.push({ name: 'SFTP side panel opened', ok: await waitFor(`!!${pick(PANEL)}?.dataset.path`, 20000) })
  const typeInTerminal = async (text) => {
    // The SSH terminal is the visible one (an earlier scenario may have left a local terminal in another tab).
    // (Hidden dock panels stay mounted at full size, only visibility: hidden — checkVisibility tells them apart.)
    await b.evaluate(`[...document.querySelectorAll('.xterm')].find((x) => x.checkVisibility({ visibilityProperty: true }))?.querySelector('.xterm-helper-textarea')?.focus()`)
    await b.send('Input.insertText', { text })
    await b.key('Enter', 'Enter', 13)
  }
  await typeInTerminal(`mkdir -p ${tree.map((d) => `~/${d}`).join(' ')} && clear && cd ~/nav`)
  const remoteHome = await (async () => {
    if (!(await waitFor(`(${pick(PANEL)}?.dataset.path || '').endsWith('/nav')`, 20000))) return null
    const p = await b.evaluate(`${pick(PANEL)}.dataset.path`)
    return p.split('/').slice(-2)[0] || null
  })()
  if (!remoteHome) {
    console.log('   SFTP panel state:', await b.evaluate(`(() => { const e = ${pick(PANEL)}; return (e ? e.dataset.path + ' pending=' + (e.dataset.pending || '') : 'no panel view') + ' | tabs: ' + [...document.querySelectorAll('.dv-tab')].map((t) => t.textContent.trim() + (t.classList.contains('dv-active-tab') ? '*' : '')).join(', ') })()`))
    if (opt.keep) writeFileSync(path.join(work, 'sftp-follow-failed.png'), Buffer.from((await b.send('Page.captureScreenshot', { format: 'png' })).data, 'base64'))
    checks.push({ name: 'SFTP panel follows the terminal into ~/nav', ok: false })
    return
  }
  await sleep(1500)
  for (const ms of latencies) {
    await setLatency(ms)
    const n = await recordNav(`SFTP panel: navigate folders (latency ${ms} ms)`, PANEL, 9, () => walk(PANEL, remoteHome, ms))
    expectIndicator('SFTP panel', ms, n)
    const cd = await recordNav(`SFTP panel: follow terminal cd (latency ${ms} ms)`, PANEL, 4, async () => {
      await typeInTerminal('cd ~/nav/alpha')
      await settle(PANEL, '/nav/alpha')
      await typeInTerminal('cd ~/nav/beta/one')
      await settle(PANEL, '/nav/beta/one')
      await typeInTerminal(`cd ~/nav/slow-cd-${ms}`)
      await settle(PANEL, `/nav/slow-cd-${ms}`)
      await typeInTerminal('cd ~/nav')
      await settle(PANEL, '/nav')
    })
    expectIndicator('SFTP panel follow cd', ms, cd)
    if (ms >= 350) await recordNav(`SFTP panel: idle, background refresh (latency ${ms} ms)`, PANEL, 0, () => sleep(9500), 500)
  }
  await setLatency(0)
}

// ---------------------------------------------------------------------------------------------------------------------
// scenarios
// ---------------------------------------------------------------------------------------------------------------------

const results = []
const checks = []

async function main() {
  writeFileSync(path.join(home, 'notes.txt'), 'NexTerm flash audit: a file for the editor caret check.\n')
  const srv = await startServer()
  const b = await startChrome()
  const latency = Number(opt.latency) || 0
  if (latency) await b.send('Network.emulateNetworkConditions', { offline: false, latency, downloadThroughput: -1, uploadThroughput: -1 })
  if (opt.motion !== 'system') await b.send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-reduced-motion', value: opt.motion }] })
  await b.send('Page.addScriptToEvaluateOnNewDocument', { source: DOM_PROBE })
  await b.send('Page.setLifecycleEventsEnabled', { enabled: true })

  let frames = []
  /** Region of interest (CSS px) while a navigation scenario records full-resolution frames. */
  let roiMode = null
  b.listeners.add((m) => {
    if (m.method !== 'Page.screencastFrame') return
    const { data, metadata, sessionId } = m.params
    void b.send('Page.screencastFrameAck', { sessionId }).catch(() => {})
    // Decoded after the recording (analyse()): decoding here would delay the acks and the other CDP events.
    frames.push({ t: metadata.timestamp * 1000, png: Buffer.from(data, 'base64'), deviceWidth: metadata.deviceWidth, roiMode })
  })
  /** Decode the recorded frames: the region grid (navigation scenarios) or the coarse + whole-viewport grids. */
  const analyse = (list) => {
    const out = []
    for (const f of list) {
      try {
        const img = decodePng(f.png)
        const scale = img.w / (f.deviceWidth || img.w)
        const png = opt.keep ? f.png : undefined
        if (f.roiMode) out.push({ t: f.t, roi: roiGrid(img, { ...f.roiMode, scale }), png })
        else out.push({ t: f.t, grid: gridOf(img), roi: roiGrid(img, { x: 0, y: 0, w: img.w / scale, h: img.h / scale, scale }), png })
      } catch {
        /* skip undecodable frame */
      }
    }
    return out
  }
  const record = async (name, fn, settle = 2500, navigation = false) => {
    frames = []
    // Navigations: frames before the new document's first paint still show the old page (see visualFlashes).
    let since = 0
    const onPaint = (m) => {
      if (m.method === 'Page.lifecycleEvent' && m.params.name === 'firstPaint' && !since) since = Date.now()
    }
    if (navigation) {
      since = 0
      b.listeners.add(onPaint)
    }
    await b.evaluate('window.__flashAudit && window.__flashAudit()').catch(() => {})
    await b.evaluate('window.__flashAuditMotion && window.__flashAuditMotion()').catch(() => {})
    await b.evaluate('window.__flashAuditStale && window.__flashAuditStale()').catch(() => {})
    await b.send('Page.startScreencast', { format: 'png', maxWidth: Number(opt.width), maxHeight: Number(opt.height), everyNthFrame: 1 })
    await fn()
    await sleep(settle)
    await b.send('Page.stopScreencast')
    const dom = ((await b.evaluate('window.__flashAudit ? window.__flashAudit() : []').catch(() => [])) || []).filter((x) => x.ms < MIN_VISIBLE)
    // A frame showing another tab's content under the active tab is a flash whatever its length (not across reloads).
    const stale = (await b.evaluate('window.__flashAuditStale ? window.__flashAuditStale() : []').catch(() => [])) || []
    if (!navigation) dom.push(...stale)
    const motion = (await b.evaluate('window.__flashAuditMotion ? window.__flashAuditMotion() : []').catch(() => [])) || []
    b.listeners.delete(onPaint)
    frames = analyse(frames)
    const settled = frames.filter((f) => !navigation || f.t >= (since || Date.now()))
    const visual = [...visualFlashes(frames, navigation ? since || Date.now() : 0), ...oscillations(settled, motion, b.inputs)]
    results.push({ name, frames: frames.length, visual, dom, since, frameList: frames })
    if (opt.keep) {
      visual.forEach((v, n) => v.frames.forEach((f, k) => f.png && writeFileSync(path.join(work, `${name.replace(/\W+/g, '-')}-${n}-${['before', 'during', 'after'][k]}.png`), f.png)))
    }
    console.log(`${visual.length || dom.length ? 'FLASH' : 'ok   '} ${name}: ${frames.length} frames, ${visual.length} visual, ${dom.length} dom` +
      (visual.length ? `\n        visual: ${visual.map((v) => (v.oscillating ? `oscillating ${v.cells} cells @${v.box}` : `${v.ms}ms/${v.cells} cells`)).join(', ')}` : '') +
      (dom.length ? `\n        dom: ${dom.map((d) => `${d.what} ${d.ms}ms`).join(', ')}` : ''))
  }
  const navigate = async (url) => {
    const l = b.loaded()
    await b.send('Page.navigate', { url })
    await l
  }
  const reload = async () => {
    const l = b.loaded()
    await b.send('Page.reload', {})
    await l
  }
  const waitFor = async (expr, ms = 20000) => {
    const end = Date.now() + ms
    for (;;) {
      if (await b.evaluate(expr).catch(() => false)) return true
      if (Date.now() > end) return false
      await sleep(100)
    }
  }
  const mac = process.platform === 'darwin'
  const MOD = mac ? 4 : 2 // Meta / Control
  const palette = async (text) => {
    await b.key('k', 'KeyK', 75, MOD)
    // Type only once the palette's field has the focus (text typed earlier would land in the terminal, and Enter would
    // run whatever the palette lists first).
    await waitFor(`!!document.activeElement?.closest?.('[cmdk-root]')`, 5000)
    await b.send('Input.insertText', { text })
    await sleep(600) // like a person reading the results: a palette closed within 400 ms of opening reads as a blink
    await b.key('Enter', 'Enter', 13)
  }

  await record('cold load (setup screen)', () => navigate(srv.base + '/'), 3000, true)
  console.log(`      (prefers-reduced-motion: ${(await b.evaluate("matchMedia('(prefers-reduced-motion: reduce)').matches")) ? 'reduce' : 'no-preference'})`)
  const pw = randomBytes(18).toString('base64url')
  const st = await b.evaluate(`fetch('/api/auth/setup',{method:'POST',headers:{'X-NexTerm':'1','Content-Type':'application/json'},body:JSON.stringify({username:'admin',password:${JSON.stringify(pw)}})}).then(r=>r.status)`)
  if (st !== 200 && st !== 201) throw new Error('setup failed: ' + st)
  await b.evaluate(`fetch('/api/auth/logout',{method:'POST',headers:{'X-NexTerm':'1'}}).then(r=>r.status)`)
  await record('launch link → shell', () => navigate(`${srv.base}/?launch=${srv.launch}`), 3500, true)
  await waitFor(`!!document.querySelector('[data-workspace]')`)
  if (!opt['nav-only']) {
    await record('open local terminal', () => b.key('L', 'KeyL', 76, MOD | 8), 3000)
    checks.push({ name: 'terminal opened', ok: await waitFor(`!!document.querySelector('.xterm-screen')`, 8000) })
    await record('reload with an open terminal', () => reload(), 3500, true)
    await record('open Local files', () => palette('Local files'), 3000)
    await record('open System information', () => palette('System information'), 3000)
    await record('open Settings', () => palette('Settings'), 3000)
    await record('switch tabs', async () => {
      for (let i = 0; i < 4; i++) {
        await b.key('Tab', 'Tab', 9, 2)
        await sleep(400)
      }
    }, 2000)
    // Mouse click at an element's centre (expr → element); Radix menus open on pointer events, not element.click().
    const clickEl = async (expr) => {
      const c = await b.evaluate(`(() => { const e = ${expr}; if (!e) return null; const r = e.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 } })()`)
      if (!c) return false
      // Like a person: the pointer rests on the target (its hover style settles) before the click.
      await b.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: c.x, y: c.y, button: 'none' })
      await sleep(450)
      for (const type of ['mousePressed', 'mouseReleased']) await b.send('Input.dispatchMouseEvent', { type, x: c.x, y: c.y, button: 'left', clickCount: 1 })
      return true
    }
    /** With --keep, a screenshot of a failed functional check (and what the page showed) for the work dir. */
    const evidence = async (name) => {
      if (!opt.keep) return
      writeFileSync(path.join(work, `check-${name.replace(/\W+/g, '-')}.png`), Buffer.from((await b.send('Page.captureScreenshot', { format: 'png' })).data, 'base64'))
      console.log(`   ${name}: tabs ${await b.evaluate(`[...document.querySelectorAll('.dv-groupview')].map((g) => '[' + [...g.querySelectorAll('.dv-tab')].map((t) => t.textContent.trim()).join(', ') + ']').join(' ')`)} | toasts ${await b.evaluate(`[...document.querySelectorAll('[data-sonner-toast]')].map((t) => t.textContent).join(' / ')`)} | dialogs ${await b.evaluate(`document.querySelectorAll('[role=dialog]').length`)} | width ${await b.evaluate('innerWidth')}`)
    }
    const tabOrder = () => b.evaluate(`[...document.querySelectorAll('.dv-active-group .dv-tab')].map((t) => t.textContent.trim()).join(' | ')`)
    const activateTab = async (n) => b.key(`F${n}`, `F${n}`, 111 + n, 2 | 1) // Ctrl+Alt+Fn: n-th tab of the group

    // Settings: grouped section navigation; switching sections (lazy pages) never blinks.
    await record('settings: switch sections', async () => {
      await b.key(',', 'Comma', 188, MOD) // settings.open
      await sleep(600)
      for (const name of ['Terminal', 'Files & SFTP', 'Keyboard shortcuts', 'Appearance']) {
        await b.evaluate(`[...document.querySelectorAll('nav[aria-label="Settings sections"] button')].find((x) => x.textContent.trim() === ${JSON.stringify(name)})?.click()`)
        await sleep(700)
      }
    }, 2000)
    checks.push({ name: 'settings navigation is grouped', ok: (await b.evaluate(`document.querySelectorAll('nav[aria-label="Settings sections"] [role=group]').length`)) >= 4 })

    // Closing a tab and reopening it (Ctrl+Shift+T) puts it back at its place, not at the end of the group.
    const before = await tabOrder()
    const sysinfoAt = await b.evaluate(`[...document.querySelectorAll('.dv-active-group .dv-tab')].findIndex((t) => !!t.querySelector('[data-tab-id="sysinfo"]')) + 1`)
    await record('close a tab and reopen it', async () => {
      await activateTab(sysinfoAt || 2)
      await sleep(500)
      await b.key('w', 'KeyW', 87, 1)
      await sleep(900)
      await b.key('T', 'KeyT', 84, MOD | 8)
      await sleep(900)
    }, 1500)
    const reopened = await tabOrder()
    checks.push({ name: `a reopened tab returns to its place (${reopened})`, ok: reopened === before })

    // Closing a running terminal and pressing Undo in the toast brings it back at its place, still connected.
    const termAt = await b.evaluate(`[...document.querySelectorAll('.dv-active-group .dv-tab')].findIndex((t) => !!t.querySelector('[data-tab-id^="terminal"]')) + 1`)
    if (termAt > 0) {
      await record('close a terminal and undo', async () => {
        await activateTab(termAt)
        await sleep(500)
        await b.key('w', 'KeyW', 87, 1)
        await sleep(900)
        await clickEl(`[...document.querySelectorAll('[data-sonner-toast] button')].find((x) => x.textContent.trim() === 'Undo')`)
        await sleep(1200)
      }, 1500)
      const undone = await tabOrder()
      checks.push({ name: `Undo brings a closed terminal back at its place (${undone})`, ok: undone === before })
      if (undone !== before) await evidence('undo')
    }

    // Phones: the group header's "all tabs" sheet instead of a truncated overflow chip.
    await b.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true })
    await sleep(1500)
    await record('phone: all tabs sheet', async () => {
      await clickEl(`document.querySelector('[aria-label^="All tabs"]')`)
      await sleep(900)
      const listed = await b.evaluate(`document.querySelectorAll('[role=dialog] li > button:first-child').length`)
      const open = await b.evaluate(`document.querySelectorAll('.dv-tab').length`)
      checks.push({ name: `phone: the tabs sheet lists every open tab (${listed} of ${open})`, ok: listed > 0 && listed === open })
      if (!listed || listed !== open) await evidence('phone-sheet')
      await clickEl(`document.querySelectorAll('[role=dialog] li > button:first-child')[0]`)
      await sleep(900)
    }, 1500)
    await b.send('Emulation.clearDeviceMetricsOverride')
    await sleep(800)

    await record('reload again', () => reload(), 3500, true)

    // Sessions panel: tag chips and grouping by tag / protocol (a few tagged sessions first).
    for (const [name, tags, protocol] of [['web-1', ['prod', 'web'], 'ssh'], ['db-1', ['prod'], 'ssh'], ['lab-vnc', [], 'vnc']]) {
      await b.evaluate(`fetch('/api/connections',{method:'POST',headers:{'X-NexTerm':'1','Content-Type':'application/json'},body:JSON.stringify(${JSON.stringify({ name, protocol, host: '192.0.2.10', username: 'audit', tags })})}).then((r) => r.status)`)
    }
    await reload()
    await sleep(2000)
    const groupBy = async (label) => {
      await clickEl(`document.querySelector('[aria-label="Sort and group sessions"]')`)
      await sleep(700)
      await clickEl(`[...document.querySelectorAll('[role=menuitemradio]')].find((x) => x.textContent.trim() === ${JSON.stringify(label)})`)
      await sleep(900)
    }
    await record('sessions: group by tag, then back to folders', async () => {
      await groupBy('Tag')
      checks.push({
        name: 'sessions: grouped by tag',
        ok: await b.evaluate(`/PROD/i.test([...document.querySelectorAll('[aria-label="Sessions by tag"] section button[aria-expanded]')].map((x) => x.textContent).join(' '))`),
      })
      await groupBy('Folders')
    }, 1500)

    // Carets: every editable surface we ship keeps a steady caret (caret-animation: manual for native fields, xterm's
    // cursorBlink off, CodeMirror cursorBlinkRate 0, Monaco cursorBlinking 'solid'). Focus it, keep still for 3.5 s:
    // a blink is an oscillation. The focus is set by script (no input event), so nothing is excluded.
    const caretCheck = async (name, focusExpr, ready) => {
      if (ready && !(await waitFor(ready, 15000))) {
        checks.push({ name: `caret: ${name} (surface not found)`, ok: false })
        return
      }
      await sleep(800)
      let focused = false
      await record(`caret: ${name} stays steady`, async () => {
        focused = await b.evaluate(`(() => { const e = ${focusExpr}; if (!e) return false; e.focus(); return document.activeElement === e || e.contains(document.activeElement) })()`)
      }, 3500)
      checks.push({ name: `caret: ${name} focused for the check`, ok: !!focused })
    }
    const visible = (sel) => `[...document.querySelectorAll(${JSON.stringify(sel)})].find((x) => x.getBoundingClientRect().width > 0 && x.checkVisibility({ visibilityProperty: true }))`
    await caretCheck('text field', `document.querySelector('input[aria-label="Search sessions"]')`)
    const termAt2 = await b.evaluate(`[...document.querySelectorAll('.dv-active-group .dv-tab')].findIndex((t) => !!t.querySelector('[data-tab-id^="terminal"]')) + 1`)
    if (termAt2 > 0) {
      await activateTab(termAt2)
      await caretCheck('terminal (xterm)', `${visible('.xterm')}?.querySelector('.xterm-helper-textarea')`, `!!${visible('.xterm-screen')}`)
    }
    await palette('Scripts')
    await sleep(800)
    await b.evaluate(`document.querySelector('[aria-label="New script"]')?.click()`)
    await caretCheck('script editor (CodeMirror)', `${visible('.cm-content')}`, `!!${visible('.cm-content')}`)
    await palette('>Local Files')
    if (await waitFor(`[...document.querySelectorAll('[data-file-browser=tab] [role=row][data-index] [role=gridcell]:first-child')].some((e) => e.textContent.trim() === 'notes.txt')`, 15000)) {
      const cell = `[...document.querySelectorAll('[data-file-browser=tab] [role=row][data-index] [role=gridcell]:first-child')].find((e) => e.textContent.trim() === 'notes.txt')`
      const c = await b.evaluate(`(() => { const r = ${cell}.getBoundingClientRect(); return { x: r.x + 30, y: r.y + r.height / 2 } })()`)
      await b.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: c.x, y: c.y, button: 'none' })
      for (const clickCount of [1, 2]) {
        await b.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: c.x, y: c.y, button: 'left', clickCount })
        await b.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: c.x, y: c.y, button: 'left', clickCount })
      }
      await caretCheck('file editor (Monaco)', `${visible('.monaco-editor')}?.querySelector('textarea')`, `!!${visible('.monaco-editor .view-lines')}`)
    } else checks.push({ name: 'caret: notes.txt listed in the files tab', ok: false })

    // Light theme: the very first painted frame of a reload must already be light (public/boot.js), not dark → light.
    const setTheme = (theme) =>
      b.evaluate(`fetch('/api/settings',{method:'PUT',headers:{'X-NexTerm':'1','Content-Type':'application/json'},body:JSON.stringify({appearance:{theme:'${theme}'}})}).then(r=>r.status)`)
    await setTheme('light')
    await reload() // the app applies and caches the light theme
    await sleep(2000)
    await record('reload in the light theme', () => reload(), 3000, true)
    const lr = results.at(-1)
    const lum = (f) => f.grid.reduce((a, v) => a + v, 0) / f.grid.length
    const firstNew = lr.frameList.find((f) => f.t >= lr.since)
    checks.push({ name: `light theme from the first frame (first frame luminance ${firstNew ? Math.round(lum(firstNew)) : '?'})`, ok: !!firstNew && lum(firstNew) > 150 })
    await setTheme('dark')
    await reload()
    await sleep(1500)

    // Functional check: pop the active tab out and dock it back into the (then empty) grid.
    const close = async () => {
      for (let i = 0; i < 12; i++) {
        const n = await b.evaluate(`document.querySelectorAll('.dv-tab').length`)
        if (n <= 1) break
        await b.key('w', 'KeyW', 87, 1) // Alt+W closes the active tab (no browser shortcut clash)
        await sleep(250)
      }
    }
    await close()
    await palette('Move Tab to New Window')
    await sleep(1500)
    await palette('Dock Tab into Grid')
    await sleep(1500)
    const docked = await b.evaluate(`(() => { const p = document.querySelector('.nx-panel'); const r = p && p.getBoundingClientRect(); return { tabs: document.querySelectorAll('.dv-tab').length, w: r ? r.width|0 : 0, h: r ? r.height|0 : 0 } })()`)
    checks.push({ name: `pop out + dock back into an empty grid (tabs ${docked.tabs}, panel ${docked.w}×${docked.h})`, ok: docked.tabs >= 1 && docked.w > 100 && docked.h > 100 })

    // Automation and recordings views: lazy pages and the first loads of their lists (palette command titles).
    for (const [name, command] of [
      ['open Automation (scripts)', 'Scripts'],
      ['automation: scheduled tasks', 'Scheduled Tasks'],
      ['automation: triggers', 'Triggers'],
      ['automation: run history', 'Automation Run History'],
      ['open the Snippets panel', 'Snippets'],
      ['open the Macros panel', 'Macros'],
      ['open Recordings', 'Open Recordings'],
    ]) {
      await record(name, () => palette(command), 3000)
    }
    checks.push({ name: 'recordings tab opened', ok: await waitFor(`[...document.querySelectorAll('[role=tab]')].some((t) => /Command history/.test(t.textContent))`, 8000) })

    // Settings → Highlighting & triggers → "Allow scripts for every user" (stored ON): after a reload the switch must
    // appear in its final state — never unchecked first and then flipped once the admin settings arrive.
    const putAdmin = (patch) =>
      b.evaluate(`fetch('/api/admin/settings',{method:'PUT',headers:{'X-NexTerm':'1','Content-Type':'application/json'},body:JSON.stringify(${JSON.stringify(patch)})}).then(r=>r.status)`)
    await putAdmin({ automation: { userScripts: true } })
    await palette('Open Settings')
    if (!(await waitFor(`!!document.querySelector('nav[aria-label="Settings sections"]')`, 8000))) await evidence('settings-not-open')
    await b.evaluate(`[...document.querySelectorAll('nav[aria-label="Settings sections"] button')].find((x) => /Highlighting/.test(x.textContent))?.click()`)
    await waitFor(`!!document.getElementById('auto-user-scripts')`, 8000)
    const { identifier: switchProbe } = await b.send('Page.addScriptToEvaluateOnNewDocument', {
      source: `(() => { const states = (window.__switchStates = []); const note = () => { const st = document.getElementById('auto-user-scripts')?.getAttribute('data-state'); if (st && states.at(-1) !== st) states.push(st) }; new MutationObserver(note).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ['data-state'] }) })()`,
    })
    if (process.env.FLASH_AUDIT_DEBUG) await evidence('before-admin-reload')
    await record('settings: admin scripts switch after a reload', () => reload(), 3000, true)
    await waitFor(`!!document.getElementById('auto-user-scripts')`, 8000)
    const switchStates = (await b.evaluate('window.__switchStates || []').catch(() => [])) || []
    if (!switchStates.length) await evidence('admin-scripts-switch')
    checks.push({ name: `admin scripts switch mounts in its final state (states: ${switchStates.join(' → ') || 'none'})`, ok: switchStates.length > 0 && switchStates.every((s) => s === 'checked') })
    await b.send('Page.removeScriptToEvaluateOnNewDocument', { identifier: switchProbe })
    await putAdmin({ automation: { userScripts: false } })

  }

  // Folder navigation in the files tab and the SFTP side panel under several latencies (see runNavigation).
  if (!latency && opt['nav-latencies'].trim()) {
    const recordNav = async (name, sel, steps, fn, settle = 1500) => {
      const roi = await b.evaluate(`(() => { const e = [...document.querySelectorAll(${JSON.stringify(sel)})].find((x) => x.getBoundingClientRect().width > 0 && x.checkVisibility({ visibilityProperty: true })); if (!e) return null; const r = e.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height } })()`)
      if (!roi) {
        checks.push({ name: `${name}: ${sel} visible`, ok: false })
        return 0
      }
      frames = []
      roiMode = roi
      await b.evaluate('window.__flashAudit && window.__flashAudit()').catch(() => {})
      await b.evaluate('window.__flashAuditMotion && window.__flashAuditMotion()').catch(() => {})
      await b.send('Page.startScreencast', { format: 'png', maxWidth: Number(opt.width), maxHeight: Number(opt.height), everyNthFrame: 1 })
      let failed = null
      try {
        await fn()
      } catch (err) {
        failed = err.message
      }
      await sleep(settle)
      await b.send('Page.stopScreencast')
      roiMode = null
      const all = ((await b.evaluate('window.__flashAudit ? window.__flashAudit() : []').catch(() => [])) || []).filter(
        (d) => d.x + d.w / 2 >= roi.x && d.x + d.w / 2 <= roi.x + roi.w && d.y + d.h / 2 >= roi.y && d.y + d.h / 2 <= roi.y + roi.h,
      )
      const dom = all.filter((d) => d.ms < NAV_MIN_VISIBLE).map((d) => ({ ...d, what: `brief ${d.what}` }))
      if (all.length > steps) dom.push({ what: `${all.length} indicator cycles for ${steps} navigation(s)`, ms: 0 })
      const motion = (await b.evaluate('window.__flashAuditMotion ? window.__flashAuditMotion() : []').catch(() => [])) || []
      frames = analyse(frames)
      const allowed = all.filter((d) => d.ms >= NAV_MIN_VISIBLE)
      const visual = [...roiFlashes(frames, allowed), ...oscillations(frames, [...allowed, ...motion], b.inputs)]
      results.push({ name, frames: frames.length, visual, dom, indicators: all.length })
      if (failed) checks.push({ name: `${name}: ${failed}`, ok: false })
      if (opt.keep) {
        visual.forEach((v, n) => v.frames.forEach((f, k) => f.png && writeFileSync(path.join(work, `${name.replace(/\W+/g, '-')}-${n}-${['before', 'during', 'after'][k]}.png`), f.png)))
      }
      console.log(`${visual.length || dom.length ? 'FLASH' : 'ok   '} ${name}: ${frames.length} frames, ${visual.length} visual, ${dom.length} dom, ${all.length} indicator(s)` +
        (all.length ? ` [${all.map((d) => `${d.ms}ms`).join(', ')}]` : '') +
        (visual.length ? `\n        visual: ${visual.map((v) => (v.oscillating ? `oscillating ${v.cells} cells @${v.box}` : `${v.ms}ms/${v.cells} cells @${v.box}`)).join(', ')}` : '') +
        (dom.length ? `\n        dom: ${dom.map((d) => `${d.what}${d.ms ? ` ${d.ms}ms` : ''}`).join(', ')}` : ''))
      return all.length
    }
    await runNavigation({ b, srv, checks, waitFor, palette, record, recordNav })
  }

  const visual = results.reduce((n, r) => n + r.visual.length, 0)
  const dom = results.reduce((n, r) => n + r.dom.length, 0)
  const failed = checks.filter((c) => !c.ok)
  for (const c of checks) console.log(`${c.ok ? 'PASS ' : 'FAIL '} ${c.name}`)
  console.log(`\nflash-audit${latency ? ` (latency ${latency} ms)` : ''}: ${visual} visual flash(es), ${dom} dom flash(es), ${failed.length} failed check(s) in ${results.length} scenarios`)
  if (opt.keep) console.log(`work dir kept: ${work}`)
  else cleanups.unshift(() => rmSync(work, { recursive: true, force: true }))
  process.exitCode = opt.strict && (visual || dom || failed.length) ? 1 : 0
}

main()
  .catch((err) => {
    console.error('flash-audit:', err.message)
    process.exitCode = 2
  })
  .finally(() => setTimeout(() => process.exit(), 300))
