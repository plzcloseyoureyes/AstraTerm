// Where downloads go (Settings → General → Downloads in the app), like a browser:
// - "Ask where to save each file" (default): a save dialog opens at once. The file meanwhile downloads to a temporary
//   file (the webview's download handler cannot wait for a dialog) and is moved to the chosen place when both are
//   done; cancelling the dialog discards it.
// - Otherwise files go straight to the download folder, never overwriting a file.
// The settings live in downloads.json in the app's config directory. The page reads and changes them through the three
// commands below, the only app commands it may call (build.rs, capabilities/downloads.json).

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};
use tauri::webview::DownloadEvent;
use tauri::{AppHandle, Manager, Webview};
use tauri_plugin_dialog::DialogExt;

#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
struct Settings {
    ask: bool,
    /// The download folder; the system's Downloads folder when unset.
    dir: Option<PathBuf>,
}

impl Default for Settings {
    fn default() -> Self {
        Self { ask: true, dir: None }
    }
}

/// A download waiting for its save dialog, its last byte, or both.
struct Pending {
    url: String,
    temp: PathBuf,
    /// The dialog's answer once given: the chosen path, or `None` when cancelled.
    chosen: Option<Option<PathBuf>>,
    finished: bool,
}

pub struct Downloads {
    file: PathBuf,
    settings: Mutex<Settings>,
    pending: Mutex<Vec<Pending>>,
}

impl Downloads {
    pub fn load(app: &AppHandle) -> Self {
        // Leftovers of downloads whose dialog was still open when the app quit.
        let _ = fs::remove_dir_all(temp_dir());
        let file = app.path().app_config_dir().unwrap_or_default().join("downloads.json");
        let settings = fs::read(&file)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default();
        Self {
            file,
            settings: Mutex::new(settings),
            pending: Mutex::new(Vec::new()),
        }
    }

    fn update(&self, change: impl FnOnce(&mut Settings)) {
        let mut settings = self.settings.lock().unwrap();
        change(&mut settings);
        if let Some(dir) = self.file.parent() {
            let _ = fs::create_dir_all(dir);
        }
        let _ = fs::write(&self.file, serde_json::to_vec_pretty(&*settings).unwrap_or_default());
    }

    /// Records the dialog's answer or the end of the transfer; once both are known, the file is moved or discarded.
    fn settle(&self, matches: impl Fn(&Pending) -> bool, change: impl FnOnce(&mut Pending)) {
        let mut pending = self.pending.lock().unwrap();
        let Some(i) = pending.iter().position(matches) else {
            return;
        };
        change(&mut pending[i]);
        if !pending[i].finished {
            return;
        }
        let Some(chosen) = pending[i].chosen.clone() else {
            return;
        };
        let done = pending.remove(i);
        match chosen {
            Some(to) => move_file(&done.temp, &to),
            None => drop(fs::remove_file(&done.temp)),
        }
    }
}

/// Where files download to while their save dialog is open.
fn temp_dir() -> PathBuf {
    std::env::temp_dir().join("astraterm-downloads")
}

fn download_dir(app: &AppHandle, settings: &Settings) -> PathBuf {
    settings
        .dir
        .clone()
        .or_else(|| app.path().download_dir().ok())
        .unwrap_or_default()
}

/// The webview's download handler (main.rs).
pub fn on_download(webview: Webview, event: DownloadEvent<'_>) -> bool {
    let app = webview.app_handle().clone();
    let state = app.state::<Downloads>();
    match event {
        DownloadEvent::Requested { url, destination } => {
            let name = destination
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .or_else(|| url.path_segments().and_then(|mut s| s.next_back()).map(str::to_owned))
                .filter(|n| !n.is_empty())
                .unwrap_or_else(|| "download".into());
            let settings = state.settings.lock().unwrap().clone();
            let dir = download_dir(&app, &settings);
            if !settings.ask {
                *destination = unique_path(&dir, &name);
                return true;
            }
            let temp_dir = temp_dir();
            if fs::create_dir_all(&temp_dir).is_err() {
                *destination = unique_path(&dir, &name);
                return true;
            }
            let temp = unique_path(&temp_dir, &name);
            *destination = temp.clone();
            state.pending.lock().unwrap().push(Pending {
                url: url.to_string(),
                temp: temp.clone(),
                chosen: None,
                finished: false,
            });
            let dialog_app = app.clone();
            app.dialog()
                .file()
                .set_parent(&webview.window())
                .set_file_name(&name)
                .set_directory(&dir)
                .save_file(move |path| {
                    let chosen = path.and_then(|p| p.into_path().ok());
                    dialog_app
                        .state::<Downloads>()
                        .settle(|p| p.temp == temp, |p| p.chosen = Some(chosen));
                });
        }
        DownloadEvent::Finished { url, success, .. } => {
            let url = url.to_string();
            state.settle(
                |p| p.url == url && !p.finished,
                |p| {
                    p.finished = true;
                    // A failed download leaves nothing to save, whatever the dialog says.
                    if !success {
                        p.chosen = Some(None);
                    }
                },
            );
        }
        _ => {}
    }
    true
}

/// `rename`, or copy + delete when the destination is on another volume.
fn move_file(from: &Path, to: &Path) {
    if fs::rename(from, to).is_err() && fs::copy(from, to).is_ok() {
        let _ = fs::remove_file(from);
    }
}

/// `dir/name`, or `dir/stem (2).ext`, `dir/stem (3).ext`… when that exists.
fn unique_path(dir: &Path, name: &str) -> PathBuf {
    let path = dir.join(name);
    if !path.exists() {
        return path;
    }
    let (stem, ext) = match name.rfind('.') {
        Some(i) if i > 0 => (&name[..i], &name[i..]),
        _ => (name, ""),
    };
    (2..)
        .map(|n| dir.join(format!("{stem} ({n}){ext}")))
        .find(|p| !p.exists())
        .unwrap()
}

/// What the settings page shows.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct View {
    ask: bool,
    dir: String,
}

fn view(app: &AppHandle) -> View {
    let settings = app.state::<Downloads>().settings.lock().unwrap().clone();
    View {
        ask: settings.ask,
        dir: download_dir(app, &settings).to_string_lossy().into_owned(),
    }
}

#[tauri::command]
pub fn download_settings(app: AppHandle) -> View {
    view(&app)
}

#[tauri::command]
pub fn set_download_ask(app: AppHandle, ask: bool) -> View {
    app.state::<Downloads>().update(|s| s.ask = ask);
    view(&app)
}

/// Lets the user pick the download folder; returns the settings (unchanged when the dialog is cancelled).
#[tauri::command]
pub async fn pick_download_dir(app: AppHandle) -> View {
    let current = download_dir(&app, &app.state::<Downloads>().settings.lock().unwrap().clone());
    let mut dialog = app.dialog().file().set_directory(current);
    if let Some(window) = app.get_webview_window("main") {
        dialog = dialog.set_parent(&window);
    }
    let picked = dialog.blocking_pick_folder();
    if let Some(dir) = picked.and_then(|p| p.into_path().ok()) {
        app.state::<Downloads>().update(|s| s.dir = Some(dir));
    }
    view(&app)
}
