# The desktop app

The desktop app (`desktop/`) is AstraTerm in a native window, built with [Tauri 2](https://tauri.app/). It does not
reimplement anything: the regular `astraterm` binary runs inside it as a sidecar, and the window shows its UI.

## How it works

1. The app opens its window on a small bundled loading page (`desktop/loading/`, styled like the web UI's boot splash)
   and starts the bundled server as `astraterm sidecar` (`cmd/astraterm/sidecar.go`), which takes the same flags as
   `astraterm serve`.
2. The sidecar runs AstraTerm in desktop mode and prints one line on stdout, `ASTRATERM_READY <url>`, once the UI can
   be loaded. The URL carries the one-time launch token, so the user is signed in automatically. The window then
   navigates there (`desktop/src-tauri/src/main.rs`).
3. The server keeps listening on its port, so a browser can connect too. When the default port (7822) is taken by
   another program, the sidecar uses a free one. If an AstraTerm instance already answers on that port, it reports
   that instance's URL instead of starting a second server.
4. The sidecar stops gracefully when its stdin closes. That happens whenever the app exits, even after a crash, so
   no orphaned server survives the app.
5. If the server cannot start or stops unexpectedly, the app shows its last log lines in a native error dialog and
   quits. The full log is `astraterm-desktop.log` in the data directory.

### Window behavior

- **Single instance** (tauri-plugin-single-instance): a second start focuses the existing window.
- **Window state** (tauri-plugin-window-state): size and position are restored.
- **Title** follows the active tab.
- **Navigation** stays on the AstraTerm server; links to anything else open in the default browser (tauri-plugin-opener).
- **`window.open`**: AstraTerm's own pages (pop-out tabs) open as native windows; other URLs (links in terminals and
  web sessions) open in the default browser.
- **Downloads** (`src/downloads.rs`; Settings → General → Downloads): by default a save dialog asks where each file
  goes (it downloads to a temporary file meanwhile); otherwise files go to the download folder, never overwriting one.
- **Translucency** (macOS, Windows): the window is transparent; the page stays opaque until Settings → Appearance →
  Window opacity is below 100%. macOS blurs behind it with vibrancy; on Windows the page turns Acrylic on only while
  see-through (it slows down moving the window on some Windows 11 builds). An initialization script tells the page
  what the window supports (`web/src/lib/desktop.ts`).
- **Title bar** (macOS, Windows): no system title bar; AstraTerm's top bar moves the window and double-click
  maximizes it (`data-tauri-drag-region`). macOS keeps its window buttons, moved into the bar; on Windows the page draws
  minimize / maximize / close (`web/src/layout/WindowControls.tsx`).
- **Security**: the page gets no Tauri IPC except its own window's title bar commands (`capabilities/window-drag.json`,
  `window-controls.json`), switching the blur on Windows (`window-effects.json`) and the three download-settings
  commands (`downloads.json`). Otherwise it runs exactly like it does in a browser.

### Engines and their limits

The window uses the operating system's web engine: WebView2 (Chromium) on Windows, WKWebView on macOS, WebKitGTK on
Linux. Everything AstraTerm needs works on all three (WebGL, WebAssembly for RDP, WebSockets, clipboard). One
exception: passkeys (WebAuthn) are not available in the macOS and Linux webviews. The desktop app signs in with its
launch token, so this only matters for server-mode instances opened in the app; use a browser for those.

### Why Tauri

- **Standard packaging for every platform**: `.app` + `.dmg`, NSIS setup `.exe` + `.msi` (with the WebView2
  bootstrapper), `.deb`, `.rpm` and `.AppImage`, all with the app icon and metadata. Code signing, notarization and
  an updater are supported when we are ready for them.
- **Small and secure**: a few MB on top of the server, the system web engine, and no IPC exposed to the page.
- **No changes to the server**: the Go binary runs unchanged as a sidecar.
- Wails was considered. It needs CGO everywhere, and its asset server cannot carry WebSockets (every terminal uses
  them), so it would load the local server URL anyway, adding build complexity without its main benefit.

## Building

Requirements: everything for `make build` (Go, Node.js), plus [Rust](https://rustup.rs/) and the
[Tauri prerequisites](https://tauri.app/start/prerequisites/) for your platform (Xcode Command Line Tools on macOS;
the WebView2 runtime and MSVC build tools on Windows; `libwebkit2gtk-4.1-dev` and friends on Linux).

```sh
make desktop                     # web UI, sidecar for this machine, then `tauri build` (all bundle types)
```

Or step by step:

```sh
make web                         # the UI embedded into the server
scripts/desktop-sidecar.sh       # desktop/src-tauri/binaries/astraterm-<target-triple>
cd desktop && npm ci && npx tauri build --bundles app,dmg     # macOS; nsis,msi on Windows; deb,rpm,appimage on Linux
```

Bundles land in `desktop/src-tauri/target/release/bundle/`. Desktop apps cannot be cross-compiled reliably, so the
release workflow builds each platform on its own runner: macOS (Apple silicon and Intel), Windows x64 and Linux x64 and
ARM64.

## Icons

All icons come from the SVG masters in `packaging/icons/`. `make icons` renders them (macOS and Chrome only): the
desktop icon set in `desktop/src-tauri/icons/` (the macOS `.icns` drawn on the macOS icon grid from
`astraterm-macos.svg`), the web manifest icons and the master PNG used for the Windows resources of `astraterm.exe`.
The results are committed.

## Next steps

- **Signing.** Apple Developer ID signing and notarization (`APPLE_*` secrets for tauri build) and Windows
  Authenticode (or Azure Trusted Signing). Until then the macOS app is ad-hoc signed, so macOS and Windows ask for a
  one-time confirmation. Before signing, set the final bundle identifier (`com.astraterm.desktop` in
  `desktop/src-tauri/tauri.conf.json`) to a domain you control.
- **Automatic updates** with tauri-plugin-updater (needs signed update artifacts and a small update manifest in the
  release).
- **Package managers**: a Homebrew cask and a winget manifest pointing at the release assets.
- **Tray icon** (optional): keep AstraTerm running with the window closed.
- **Third-party notices for the Rust crates** bundled into the desktop app (for example with `cargo about`).
