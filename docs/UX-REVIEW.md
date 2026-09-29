# UX review — core flows (2026-09-28)

Walk-through of the most-used flows as a power user would do them, on a throwaway build (temporary HOME and data dir,
own `*.localhost` host, headless Chrome at 1440×900, 1024×768 and 390×844, dark and light), against the Docker lab
(`ssh1`): first run → saved sessions → connect SSH → SFTP side panel → edit a file → monitoring bar → split panes /
MultiExec → tunnels → tools → settings → reload / reattach. Findings are ordered by impact. "Done" marks what this
pass changed (details in SPEC §9 "UX refinement pass"); the rest is the backlog.

## P0 — things that feel broken

1. **Flashing in the file browser** (owner's complaint). Opening a folder showed a full-width 2 px bar for ≥ 400 ms,
   dimmed the old rows, faded the new rows in from blank, blinked a "Refreshing" spinner in the status line on every
   poll (every 8 s), on every followed `cd` and on most navigations at 150–700 ms latency, and moved the cursor ring
   onto the rows being left. Measured by the extended flash audit: 80 visual + 59 DOM flashes in 20 navigation
   scenarios on the old UI. **Done** — 0 / 0: cached + prefetched listings, previous rows kept, one delayed spinner in
   the location bar (navigation preset 1000 ms / ≥ 800 ms), nothing for background refreshes.
2. **Things that pulse or blink** — the amber "connecting" dot breathed (tabs, session tree, status bar, Home, SFTP
   panel, tunnels, servers, VNC / RDP toolbars), a heartbeat ring on live data, a sweeping indeterminate bar, a blinking
   terminal cursor, a pulsing MultiExec frame and a screen-flash visual bell. **Done** — steady ring for pending, solid
   dots for settled states, still indeterminate bar, steady cursor by default, steady MultiExec frame, bell mark;
   looping animations removed from the theme and banned by `lint-ui`; the audit detects oscillation on screen.
3. **Short-lived indicators everywhere** (400 ms minimum read as a blink; badges for transfers, pastes, input lock,
   transport blips, ended-session panels appeared at once). **Done** — 300 / 600 ms default, delayed badges,
   `useSteadyStatus` so a quick reconnect never swaps green → amber → green.

## P1 — friction in everyday work

4. **Closing a running session asks every time** (modal), yet nothing can be undone. **Done** — the tab closes at once;
   the session ends after 6 s with one "Undo" toast (terminal, VNC, RDP); the old confirmation stays available as a
   setting.
5. **Toolbar silently loses buttons** at ≤ 1280 px (Keys, Settings, Help scrolled out of sight with no scrollbar); 13
   ungrouped buttons with two different chevron styles. **Done** — grouped toolbar (connect · workspace · network ·
   tools · app), overflow "More" menu, one chevron language.
6. **Quick connect only remembers raw specs** — typing a saved session's name found nothing. **Done** — fuzzy
   suggestions of saved sessions (name / host / user / tags) + history; Enter opens the highlighted session.
7. **No way to keep a working set of tabs** (e.g. "prod triage": 3 SSH + SFTP + tunnels, split). **Done** — saved
   workspaces (View / Split → Workspaces, palette, Home); restoring reconnects sessions that no longer run.
8. **Home is a static dashboard** with two quick-connect fields visible at once and nothing about how sessions are
   organized. **Done** — launcher-style Home: large quick connect, Recent (hover: open right / files / edit), Running,
   Workspaces, Organize (folders with counts, tags → filtered tree). **Done (final pass)**: one field on screen — the
   toolbar's when it shows one (Home then offers a "Quick connect" card), Home's large field otherwise.
9. **Host-key prompt repeats itself** (fields + the same text again; "Session: ssh1 (lab) (test@…)"). **Done**.
10. **Light theme contrast**: success 4.35:1, info 4.1:1, warning 2.75:1 on white (≈ 3.8 / 3.7 / 2.4 on the status
    bar). **Done** — ≥ 4.5:1 on white and on the status bar.

## P2 — polish and consistency (backlog)

11. ~~Undo reopens the tab at the end of its group.~~ **Done (final pass)**: closed tabs remember their neighbours;
    Undo / Reopen puts them back in place (a burst comes back in its order).
12. ~~Tab strip on phones: the overflow chip truncates labels.~~ **Done (final pass)**: an "All tabs" bottom sheet.
13. ~~Status bar running-sessions item is icon + number only.~~ **Done (final pass)**: "2 sessions" on wide screens.
14. Sidebar rail has a Settings gear while the toolbar and menu bar have Settings too; the rail could host
    user-configurable panels instead.
15. ~~Session tree: tags only visible as filters; folder colours not on the tree's count badges.~~ **Done (final
    pass)**: tag chips on hovered / selected rows (click filters), tinted folder counts, Tags / Colour for multi-selections,
    group by protocol or tag.
16. ~~The Settings tab lists 20 sections in one column.~~ **Done (final pass)**: grouped navigation (General ·
    Appearance · Terminal & Editor · Connections · Files · Security · Integrations · About) with search.
17. Command palette: no "recent sessions" group before typing (only recent commands); the Home launcher now covers it.
18. Other modules (owned elsewhere) still flash in the audit at times: opening the Snippets / Macros / Recordings panels
    (≈ 400 ms panel placeholders) — reported to their owners; the audit catches them.

## Measurements

| Scenario (flash audit) | old UI | now |
|---|---|---|
| Files tab navigation, 0/150/350/700/1500 ms | 35 visual, 23 DOM | 0 / 0 (one ≥ 800 ms spinner only at 1500 ms) |
| SFTP panel navigation, same latencies | 32 visual, 23 DOM | 0 / 0 |
| SFTP follow terminal `cd` | 11 visual, 8 DOM | 0 / 0 |
| SFTP idle with background refresh | 2 visual, 5 DOM | 0 / 0 |
| SSH connect / reconnect (new) | — | 0 / 0 |

Screenshots of this pass: the scratchpad `uxref/shots/` folder of the session (not committed).
