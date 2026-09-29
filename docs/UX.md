# NexTerm UX principles (binding for every UI change)

NexTerm is an **organized remote-management workspace**: every server, session, file system, tunnel and remote desktop
in one self-hosted place, structured by folders, tags, identities and saved layouts, reachable from a single binary.
Familiar layout (toolbar, left sidebar with Sessions/Files, tabbed workspace, status/monitoring bar), but calmer,
smarter and more interactive than traditional terminal suites. The owner's words: "refine, refine, refine" — polish,
organization, ease of use and interactivity first. Public text (UI strings, docs, README) never references other
products by name, except as a supported *import format* (e.g. "Import from MobaXterm, PuTTY, ~/.ssh/config").

## Motion & feedback — zero flashing (owner's #1 visual requirement)
- **No oscillating or blinking animation anywhere**: no pulse, breathe, blink, heartbeat or fade-in/out loops on dots,
  icons, badges, text or bars. `animate-pulse-dot` / `animate-heartbeat` are retired. Status is conveyed by a **steady**
  color + shape + short text (e.g. connecting = steady amber ring outline + "Connecting…" in tooltip/status text;
  connected = solid green dot; error = solid red dot). A state change swaps the dot once, without animation.
- **Nothing appears and disappears quickly.** Short operations show no indicator at all. Indicators for navigation-like
  operations (opening a folder, switching views) appear only after **1000 ms** and then stay **≥ 800 ms**; for
  operations the user explicitly waits on (connect, upload, save) after 300 ms / ≥ 600 ms. Never more than one
  indicator cycle per user action; bursts of actions coalesce.
- Prefer instant UI over indicators: caches, prefetch, optimistic updates, keeping the previous content on screen.
- The only allowed continuous motion: a slow, smooth rotating ring for an explicit long wait, and determinate progress
  bars that fill monotonically. Transitions 120–200 ms ease-out, no layout shift, `tabular-nums` for changing numbers.
- Carets are steady too: native fields through `caret-animation: manual` (index.css; Chrome / Edge ≥ 139 — other
  engines ignore it and keep their native blink), xterm `cursorBlink: false` by default (a user setting may turn it
  on), Monaco `cursorBlinking: 'solid'`, CodeMirror `drawSelection({ cursorBlinkRate: 0 })`. `lint-ui` rule `caret`
  and the flash audit's caret scenarios enforce it.
- Respect `prefers-reduced-motion` (already global): no transitions, one-shot animations end at once, the spinner is a
  still ring.

## Loading states (binding; enforced by `make lint-ui`)
One system, in the shared layer — features never hand-roll spinners, skeletons, progress bars or the timing hook.

**Rules**
1. *Nothing for fast operations.* An indicator appears only after the operation has run **300 ms** (navigation-like: **1000 ms**); once shown it
   stays **≥ 600 ms** (navigation-like: **≥ 800 ms**) (`lib/useDelayedFlag`). Keep indicators *mounted* and toggle them (`<Spinner active={busy}>`),
   or the minimum time cannot apply; a component that returns early while loading uses `useLoadingGate`.
2. *Skeletons only for a first load* (no data yet), after the delay. **Never on a refetch or background refresh**:
   the old content stays on screen. react-query keeps the previous data while a key changes
   (`placeholderData: keepPreviousData` in `api/queryClient.ts`); queries whose old data would be misleading under a
   new key (another host's processes, another connection's settings) opt out with `placeholderData: undefined`.
3. *Background work is quiet*: polling, follow-cwd, auto-refresh and event-driven refetches show at most a small
   delayed inline spinner in a reserved slot (`<Spinner active={q.isFetching} reserve />`). Refresh icons spin only
   for a refresh the user asked for (`useManualRefresh` → `<IconButton busy>` / `<BusyIcon busy>`).
4. *Errors* while there is no data use `ErrorState` (message + "Try again"); an error during a refetch keeps the data.
5. *Progress* is monotonic and eased (`ProgressBar` with a `resetKey`); indeterminate only when truly unknown.
6. *Status dots* are always steady — `pending` (connecting, reconnecting, starting) is a steady amber ring, never an animation.

**Primitives** (`web/src/components/ui`, `web/src/lib`)
| API | Use |
|---|---|
| `useDelayedFlag(busy, {delay?, minVisible?})`, `DELAY_PRESETS.{EXPLICIT_WAIT,NAVIGATION}` | the timing rule as a boolean (custom indicators); presets 300/600 (default) and 1000/800 ms |
| `useLoadingGate(busy)` → `{hold, show}` | early returns: `if (g.hold) return g.show ? <LoadingPane immediate /> : null` |
| `<Spinner active? immediate? reserve? label? />` | inline spinner, delayed by default; `immediate` inside an already delayed region |
| `<Delayed active? fallback?>…</Delayed>` | any indicator (spinner + text, overlay card) under the rule |
| `<LoadingPane active? label? />` | centered pane spinner (lazy tabs, first loads) |
| `<BusyIcon icon busy />`, `<IconButton busy>`, `<Button loading>` | icons / buttons for slow user actions |
| `useManualRefresh(refetch)` → `{refreshing, refresh}` | tells a user refresh from polling |
| `<LoadingState busy skeleton?>…</LoadingState>` | first-load gate for non-query content |
| `<QueryState query skeleton? empty? isEmpty? error?>{(data) => …}</QueryState>` | loading / empty / error / content for a query |
| `<Skeleton />`, `<SkeletonRows />`, `<SkeletonText />` | static, low-contrast placeholders (only inside the gates above) |
| `<ProgressBar value resetKey? tone? />` | monotonic eased bar; no `value` = a still, softly tinted bar (nothing sweeps) |
| `<StatusDot tone pending? />`, `statusDotClass(tone, pending)` | steady status dots; `pending` = steady ring outline (`pendingPulseClass` is deprecated and empty) |
| `useSteadyStatus(value, isPending)` | the status to display: pending blips < 300 ms keep the previous dot (no green → amber → green) |

**Boot.** `public/boot.js` applies the cached theme / accent / scale before the first paint; index.html carries a
static splash identical to the React one (`.nx-splash`, a still mark, no pulse) that stays one mounted instance through
auth, settings and the AppShell chunk (preloaded in parallel with the auth request) and fades out once (150 ms). UI and
terminal fonts are preloaded and use `font-display: block` (no swap reflow). `make flash-audit` measures all of this.

`scripts/lint-ui.mjs` fails on raw `animate-spin|pulse|pulse-dot|indeterminate` classes, ad-hoc skeleton components
and private copies of the timing hook outside `components/ui` and `lib`, and — everywhere, the shared layer included —
on oscillating motion (`oscillation`: pulse / ping / bounce / blink / breathe / heartbeat / indeterminate classes,
keyframes named like them, any `infinite` animation other than the spinner's `spin`, `repeat-infinite`). The looping
Tailwind animations are removed from the theme (`index.css`), so those classes do nothing anyway (escape hatch: a
`lint-ui-allow: <reason>` comment; `scripts/lint-ui-allowlist.json` is empty and may only stay so).
`make flash-audit` also flags oscillation on screen (a cell that keeps returning to a previous level, gradually or by
toggling) and, in the folder-navigation scenarios, any small indicator that appears and disappears.

## Interaction
- Keyboard-first: every action reachable from the command palette; visible shortcuts in menus/tooltips; focus is
  always visible and never lost after dialogs close.
- Direct manipulation: drag & drop wherever it makes sense (files, tabs, sessions, snippets onto terminals), inline
  rename, right-click everywhere with context-appropriate actions, double-click = primary action.
- Progressive disclosure: simple by default, advanced options behind "Advanced" sections; smart defaults so most
  connections need only host + user.
- Forgiving: undo toasts for destructive actions where feasible (delete session → "Undo"), confirmations only for truly
  irreversible or remote-destructive actions, never lose user input (drafts survive close/reload).
- Helpful empty states with one clear primary action; errors say what happened and what to do next (with a retry /
  fix button), never raw stack traces.
- Instant feedback: optimistic updates for local state; toasts are brief, stack neatly and don't cover the controls
  the user needs next.

## Visual
- Dense but breathable IDE look, 13 px base, consistent 4 px spacing grid, one accent color, semantic colors only for
  meaning (success/warning/destructive/info). Dark and light both first-class; check contrast ≥ 4.5:1 for text.
- Use the shared primitives in `web/src/components/ui/*` and tokens in `web/src/index.css` — no one-off colors,
  shadows or radii.
- Icons from lucide, consistently sized (14/16 px in dense UI).
- Numbers that update live use `tabular-nums` and never change width.

## Editor
- The file editor is Monaco (bundled locally, workers via Vite, no CDN), themed from our design tokens, lazy-loaded.
