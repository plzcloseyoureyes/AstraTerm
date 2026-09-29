/*
 * Pre-paint boot (docs/UX.md "Loading states"): a tiny blocking same-origin script (the CSP allows 'self' scripts
 * only) that applies the cached appearance — dark / light / system, accent, UI scale, density — before the first
 * paint, so a light-theme user never sees the dark default and the static splash in index.html already has the right
 * colours. Keep it in sync with applyTo() in src/lib/theme.ts (same localStorage key and CSS variables).
 */
;(function () {
  var root = document.documentElement
  var a = null
  try {
    a = JSON.parse(localStorage.getItem('nexterm:appearance') || 'null')
  } catch (e) {
    a = null
  }
  a = a && typeof a === 'object' ? a : {}
  var theme = a.theme === 'light' || a.theme === 'system' ? a.theme : 'dark'
  var dark = theme === 'dark' || (theme === 'system' && (!window.matchMedia || window.matchMedia('(prefers-color-scheme: dark)').matches))
  root.classList.toggle('dark', dark)
  root.style.colorScheme = dark ? 'dark' : 'light'
  var v = a.vars && a.vars[dark ? 'dark' : 'light']
  if (v && typeof v.primary === 'string' && typeof v.fg === 'string') {
    root.style.setProperty('--primary', v.primary)
    root.style.setProperty('--primary-foreground', v.fg)
  }
  var scale = typeof a.uiScale === 'number' && isFinite(a.uiScale) ? Math.min(2, Math.max(0.7, a.uiScale)) : 1
  root.style.setProperty('--ui-scale', String(scale))
  if (a.density === 'compact') root.style.setProperty('--spacing', '0.2rem')
  root.dataset.density = a.density === 'compact' ? 'compact' : 'comfortable'
  var meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', dark ? '#15181d' : '#f7f8fa')
})()
