// AstraTerm desktop app: a native window (the system webview) around the AstraTerm server.
//
// The server is the regular `astraterm` binary, bundled as a Tauri sidecar and started with `astraterm sidecar`
// (cmd/astraterm/sidecar.go). It prints `ASTRATERM_READY <url>` once the UI can be loaded, then the window navigates
// there. It keeps listening on its port (browsers can connect too) and stops gracefully when its stdin closes, which
// happens whenever this app exits, even if it crashes.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")] // no console window on Windows

use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use tauri::webview::{DownloadEvent, NewWindowResponse};
use tauri::{AppHandle, Manager, Runtime, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
use tauri_plugin_opener::OpenerExt;
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;
use tauri_plugin_window_state::StateFlags;
use url::Url;

const READY_PREFIX: &str = "ASTRATERM_READY ";
/// Server log lines kept for the error dialog when the server cannot start.
const LOG_TAIL: usize = 12;

/// The running server, held for the app's lifetime: dropping it would close its stdin, which stops the server.
struct Sidecar(#[allow(dead_code)] Mutex<Option<CommandChild>>);

/// The server's origin once known (navigation and pop-up policy).
#[derive(Default, Clone)]
struct Origin(Arc<Mutex<Option<url::Origin>>>);

impl Origin {
    fn is_app(&self, url: &Url) -> bool {
        match self.0.lock().unwrap().as_ref() {
            Some(origin) => &url.origin() == origin,
            None => false,
        }
    }
}

fn main() {
    tauri::Builder::default()
        // A second start focuses the running window instead of starting another server.
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.unminimize();
                let _ = window.set_focus();
            }
        }))
        // Size, position and maximized state; never the decorations, which title_bar() owns (a restored system frame
        // would sit on top of AstraTerm's own title bar).
        .plugin(
            tauri_plugin_window_state::Builder::default()
                .with_state_flags(StateFlags::all() - StateFlags::DECORATIONS)
                .build(),
        )
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .setup(|app| {
            let origin = Origin::default();
            let window = main_window(app.handle(), origin.clone())?;
            start_server(app.handle(), window, origin)?;
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running AstraTerm");
}

/// The main window: the bundled loading page first, then the server's UI.
fn main_window(app: &AppHandle, origin: Origin) -> tauri::Result<WebviewWindow> {
    let nav_origin = origin.clone();
    let nav_app = app.clone();
    let popup_app = app.clone();
    let popup_origin = origin;
    let window = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()));
    title_bar(translucent(window))
        .title("AstraTerm")
        .inner_size(1440.0, 900.0)
        .min_inner_size(820.0, 560.0)
        .center()
        // The page handles drag and drop itself (tabs, files onto the file browser and terminals). Tauri's native
        // handler would swallow those HTML5 events on Windows (WebView2).
        .disable_drag_drop_handler()
        // The window title follows the active tab ("host — AstraTerm").
        .on_document_title_changed(|window, title| {
            let _ = window.set_title(&title);
        })
        // Stay on AstraTerm: links to anything else open in the default browser.
        .on_navigation(move |url| {
            if nav_origin.is_app(url)
                || matches!(url.scheme(), "tauri" | "about" | "data" | "blob")
                || url.host_str() == Some("tauri.localhost")
            {
                return true;
            }
            let _ = nav_app.opener().open_url(url.as_str(), None::<&str>);
            false
        })
        // window.open: AstraTerm's own pages (pop-out tabs) get a native window; other URLs (links in terminals and
        // web sessions) open in the default browser.
        .on_new_window(move |url, features| {
            if !popup_origin.is_app(&url) {
                let _ = popup_app.opener().open_url(url.as_str(), None::<&str>);
                return NewWindowResponse::Deny;
            }
            static NEXT: AtomicUsize = AtomicUsize::new(1);
            let label = format!("popout-{}", NEXT.fetch_add(1, Ordering::Relaxed));
            let url = WebviewUrl::External("about:blank".parse().unwrap());
            let built = translucent(WebviewWindowBuilder::new(&popup_app, label, url))
                .window_features(features)
                .disable_drag_drop_handler()
                .title("AstraTerm")
                .on_document_title_changed(|window, title| {
                    let _ = window.set_title(&title);
                })
                .build();
            match built {
                Ok(window) => NewWindowResponse::Create { window },
                Err(_) => NewWindowResponse::Deny,
            }
        })
        // Downloads (files, exports, recordings) go to the Downloads folder, never overwriting a file.
        .on_download(|webview, event| {
            if let DownloadEvent::Requested { url, destination } = event {
                let name = destination
                    .file_name()
                    .map(|n| n.to_string_lossy().into_owned())
                    .or_else(|| url.path_segments().and_then(|mut s| s.next_back()).map(str::to_owned))
                    .filter(|n| !n.is_empty())
                    .unwrap_or_else(|| "download".into());
                if let Ok(dir) = webview.app_handle().path().download_dir() {
                    *destination = unique_path(&dir, &name);
                }
            }
            true
        })
        .build()
}

/// macOS and Windows: a transparent window. The page stays opaque unless Settings → Appearance → Window opacity is
/// below 100% (web/src/lib/theme.ts); the script tells it what the window supports.
/// - macOS: always over the vibrancy blur (free when covered by the opaque page).
/// - Windows: the page turns Acrylic on only while it is see-through (capabilities/window-effects.json), since Acrylic
///   slows down moving and resizing the window on some Windows 11 builds.
fn translucent<'a, R: Runtime, M: Manager<R>>(
    builder: WebviewWindowBuilder<'a, R, M>,
) -> WebviewWindowBuilder<'a, R, M> {
    #[cfg(target_os = "macos")]
    {
        use tauri::window::{Effect, EffectState, EffectsBuilder};
        // The HUD material is the clearest of the dark blurs; always active, so it does not turn grey when unfocused.
        let effects = EffectsBuilder::new()
            .effect(Effect::HudWindow)
            .state(EffectState::Active)
            .build();
        builder
            .transparent(true)
            .effects(effects)
            .initialization_script("window.__ASTRATERM_DESKTOP__ = { translucent: true };")
    }
    #[cfg(target_os = "windows")]
    {
        builder
            .transparent(true)
            .initialization_script("window.__ASTRATERM_DESKTOP__ = { translucent: true, blurOnDemand: true };")
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    builder
}

/// No system title bar: the page's top bar drags the window (data-tauri-drag-region). macOS keeps its window buttons,
/// moved into that bar; on Windows the page draws its own (web/src/layout/WindowControls.tsx). The capabilities
/// window-drag.json and window-controls.json allow exactly those window commands.
fn title_bar<'a, R: Runtime, M: Manager<R>>(builder: WebviewWindowBuilder<'a, R, M>) -> WebviewWindowBuilder<'a, R, M> {
    #[cfg(target_os = "macos")]
    {
        builder
            .title_bar_style(tauri::TitleBarStyle::Overlay)
            .hidden_title(true)
            .traffic_light_position(tauri::LogicalPosition::new(16.0, 20.0))
            .initialization_script(
                "window.__ASTRATERM_DESKTOP__ = { ...window.__ASTRATERM_DESKTOP__, titleBar: 'macos' };",
            )
    }
    #[cfg(target_os = "windows")]
    {
        builder.decorations(false).initialization_script(
            "window.__ASTRATERM_DESKTOP__ = { ...window.__ASTRATERM_DESKTOP__, titleBar: 'windows' };",
        )
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    builder
}

/// Starts the bundled server and points the window at it once it is ready. If it cannot start (or stops), the
/// reason is shown in a dialog and the app quits.
fn start_server(app: &AppHandle, window: WebviewWindow, origin: Origin) -> tauri::Result<()> {
    let command = app
        .shell()
        .sidecar("astraterm")
        .map_err(|e| tauri::Error::Anyhow(e.into()))?
        .arg("sidecar");
    let (mut events, child) = command.spawn().map_err(|e| tauri::Error::Anyhow(e.into()))?;
    app.manage(Sidecar(Mutex::new(Some(child))));

    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let mut tail: VecDeque<String> = VecDeque::with_capacity(LOG_TAIL);
        let mut ready = false;
        while let Some(event) = events.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    let line = String::from_utf8_lossy(&line);
                    if let Some(raw) = line.trim().strip_prefix(READY_PREFIX) {
                        if let Ok(url) = Url::parse(raw) {
                            *origin.0.lock().unwrap() = Some(url.origin());
                            let _ = window.navigate(url);
                            ready = true;
                        }
                    }
                }
                CommandEvent::Stderr(line) => {
                    let line = String::from_utf8_lossy(&line).trim_end().to_owned();
                    eprintln!("{line}");
                    if tail.len() == LOG_TAIL {
                        tail.pop_front();
                    }
                    tail.push_back(line);
                }
                CommandEvent::Terminated(status) => {
                    let what = if ready {
                        "AstraTerm stopped unexpectedly"
                    } else {
                        "AstraTerm could not start"
                    };
                    let log: Vec<String> = tail.iter().cloned().collect();
                    let message = format!(
                        "The AstraTerm server exited (code {}).\n\n{}",
                        status.code.map_or("unknown".into(), |c| c.to_string()),
                        log.join("\n")
                    );
                    app.dialog()
                        .message(message)
                        .title(what)
                        .kind(MessageDialogKind::Error)
                        .blocking_show();
                    app.exit(1);
                    break;
                }
                _ => {}
            }
        }
    });
    Ok(())
}

/// dir/name, or dir/"name (2).ext" … when that file exists.
fn unique_path(dir: &Path, name: &str) -> PathBuf {
    let candidate = dir.join(name);
    if !candidate.exists() {
        return candidate;
    }
    let (stem, ext) = match name.rsplit_once('.') {
        Some((s, e)) if !s.is_empty() => (s.to_owned(), format!(".{e}")),
        _ => (name.to_owned(), String::new()),
    };
    (2..)
        .map(|n| dir.join(format!("{stem} ({n}){ext}")))
        .find(|p| !p.exists())
        .unwrap()
}
