fn main() {
    // The app's own commands, so capabilities can grant exactly these to the page (capabilities/*.json).
    let app = tauri_build::AppManifest::new().commands(&[
        "download_settings",
        "set_download_ask",
        "pick_download_dir",
        "new_window",
        "set_hardware_acceleration",
    ]);
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(app)).expect("failed to run tauri-build");
}
