// Preferences the desktop app needs before its first window exists (prefs.json in the app's config directory). The
// page owns them (Settings → Appearance) and copies them here through set_hardware_acceleration; they apply at the
// next start.

use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager};

#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Prefs {
    /// Off: the web engine draws without the GPU (Windows: --disable-gpu; Linux: no WebKit compositing).
    pub hardware_acceleration: bool,
}

impl Default for Prefs {
    fn default() -> Self {
        Self {
            hardware_acceleration: true,
        }
    }
}

fn file(app: &AppHandle) -> PathBuf {
    app.path().app_config_dir().unwrap_or_default().join("prefs.json")
}

pub fn load(app: &AppHandle) -> Prefs {
    fs::read(file(app))
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

/// Before any webview exists: Linux reads these when WebKit starts.
pub fn apply_early(prefs: &Prefs) {
    #[cfg(target_os = "linux")]
    if !prefs.hardware_acceleration {
        std::env::set_var("WEBKIT_DISABLE_COMPOSITING_MODE", "1");
        std::env::set_var("WEBKIT_DISABLE_DMABUF_RENDERER", "1");
    }
    #[cfg(not(target_os = "linux"))]
    let _ = prefs;
}

/// Windows: WebView2 takes the GPU switch as a browser argument (the same for every window of the app). Tauri's own
/// default arguments are kept.
#[cfg(target_os = "windows")]
pub fn webview_args(prefs: &Prefs) -> Option<&'static str> {
    if prefs.hardware_acceleration {
        None
    } else {
        Some("--disable-features=msWebOOUI,msPdfOOUI,msSmartScreenProtection --disable-gpu")
    }
}

#[tauri::command]
pub fn set_hardware_acceleration(app: AppHandle, enabled: bool) {
    let mut prefs = load(&app);
    if prefs.hardware_acceleration == enabled {
        return;
    }
    prefs.hardware_acceleration = enabled;
    let path = file(&app);
    if let Some(dir) = path.parent() {
        let _ = fs::create_dir_all(dir);
    }
    let _ = fs::write(path, serde_json::to_vec_pretty(&prefs).unwrap_or_default());
}
