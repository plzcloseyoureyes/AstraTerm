// "New Window" where the operating system offers it, like VS Code or a terminal app: the macOS File menu (⌘N) and
// Dock menu, and the Windows taskbar jump list. Linux launchers get it from the desktop entry
// (linux/astraterm.desktop). All of them, and a second start of the app, end up in main.rs's open_window.

use tauri::AppHandle;

/// Menu item id of New Window.
pub const NEW_WINDOW: &str = "new-window";

/// Command-line flag of the jump list task, like the Linux desktop action's (a second start opens a window anyway).
#[cfg(target_os = "windows")]
const NEW_WINDOW_FLAG: &str = "--new-window";

#[cfg(target_os = "macos")]
pub fn install(app: &AppHandle) -> tauri::Result<()> {
    app_menu(app)?;
    dock_menu();
    Ok(())
}

#[cfg(target_os = "windows")]
pub fn install(_app: &AppHandle) -> tauri::Result<()> {
    if let Err(err) = jump_list() {
        eprintln!("jump list: {err}");
    }
    Ok(())
}

#[cfg(not(any(target_os = "macos", target_os = "windows")))]
pub fn install(_app: &AppHandle) -> tauri::Result<()> {
    Ok(())
}

/// The standard macOS menu bar with File → New Window (⌘N) first.
#[cfg(target_os = "macos")]
fn app_menu(app: &AppHandle) -> tauri::Result<()> {
    use tauri::menu::{Menu, MenuItem, MenuItemKind};
    let menu = Menu::default(app)?;
    let item = MenuItem::with_id(app, NEW_WINDOW, "New Window", true, Some("CmdOrCtrl+N"))?;
    for kind in menu.items()? {
        if let MenuItemKind::Submenu(sub) = kind {
            if sub.text()? == "File" {
                sub.insert(&item, 0)?;
            }
        }
    }
    app.set_menu(menu)?;
    Ok(())
}

/// Adds New Window to the Dock icon's menu. AppKit asks the application delegate for it (applicationDockMenu:), and
/// Tauri has no API for that, so the method is added to the delegate's class. Its item reports through the same menu
/// events as the menu bar.
#[cfg(target_os = "macos")]
fn dock_menu() {
    use std::ffi::c_void;
    use std::sync::OnceLock;

    use muda::{ContextMenu, Menu, MenuItem};
    use objc2::runtime::{AnyClass, AnyObject, Imp, Sel};
    use objc2::{sel, MainThreadMarker};
    use objc2_app_kit::NSApplication;

    static DOCK_MENU: OnceLock<usize> = OnceLock::new();

    extern "C" fn dock_menu(_this: *mut AnyObject, _cmd: Sel, _sender: *mut AnyObject) -> *mut c_void {
        DOCK_MENU.get().map_or(std::ptr::null_mut(), |p| *p as *mut c_void)
    }

    let Some(mtm) = MainThreadMarker::new() else { return };
    let menu = Menu::new();
    if menu
        .append(&MenuItem::with_id(NEW_WINDOW, "New Window", true, None))
        .is_err()
    {
        return;
    }
    let _ = DOCK_MENU.set(menu.ns_menu() as usize);
    std::mem::forget(menu); // lives as long as the app

    let app = NSApplication::sharedApplication(mtm);
    let Some(delegate) = app.delegate() else { return };
    let object: *const AnyObject = objc2::rc::Retained::as_ptr(&delegate).cast();
    // SAFETY: the delegate is a live Objective-C object; the method has the delegate signature (id self, SEL, id) -> id.
    unsafe {
        let class: *const AnyClass = (*object).class();
        let imp: Imp =
            std::mem::transmute::<extern "C" fn(*mut AnyObject, Sel, *mut AnyObject) -> *mut c_void, Imp>(dock_menu);
        objc2::ffi::class_addMethod(class.cast_mut(), sel!(applicationDockMenu:), imp, c"@@:@".as_ptr());
    }
}

/// A "New Window" task in the taskbar button's jump list: it starts the app again with --new-window, which the
/// running instance turns into a new window.
#[cfg(target_os = "windows")]
fn jump_list() -> windows::core::Result<()> {
    use windows::core::{Interface, HSTRING};
    use windows::Win32::Storage::EnhancedStorage::PKEY_Title;
    use windows::Win32::System::Com::StructuredStorage::{PropVariantChangeType, PROPVARIANT, PVCHF_DEFAULT};
    use windows::Win32::System::Com::{
        CoCreateInstance, CoInitializeEx, CLSCTX_INPROC_SERVER, COINIT_APARTMENTTHREADED,
    };
    use windows::Win32::System::Variant::VT_LPWSTR;
    use windows::Win32::UI::Shell::Common::{IObjectArray, IObjectCollection};
    use windows::Win32::UI::Shell::PropertiesSystem::IPropertyStore;
    use windows::Win32::UI::Shell::{
        DestinationList, EnumerableObjectCollection, ICustomDestinationList, IShellLinkW, ShellLink,
    };

    let exe = std::env::current_exe().map_err(|_| windows::core::Error::from_thread())?;
    let exe = HSTRING::from(exe.as_os_str());
    // SAFETY: plain COM calls on this thread; every interface is released when dropped.
    unsafe {
        let _ = CoInitializeEx(None, COINIT_APARTMENTTHREADED);
        let list: ICustomDestinationList = CoCreateInstance(&DestinationList, None, CLSCTX_INPROC_SERVER)?;
        let mut max_slots = 0u32;
        let _removed: IObjectArray = list.BeginList(&mut max_slots)?;

        let link: IShellLinkW = CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER)?;
        link.SetPath(&exe)?;
        link.SetArguments(&HSTRING::from(NEW_WINDOW_FLAG))?;
        link.SetIconLocation(&exe, 0)?;
        // The task's label: the shell reads PKEY_Title as a wide string (VT_LPWSTR).
        let mut title = PROPVARIANT::default();
        PropVariantChangeType(&mut title, &PROPVARIANT::from("New Window"), PVCHF_DEFAULT, VT_LPWSTR)?;
        let store: IPropertyStore = link.cast()?;
        store.SetValue(&PKEY_Title, &title)?;
        store.Commit()?;

        let tasks: IObjectCollection = CoCreateInstance(&EnumerableObjectCollection, None, CLSCTX_INPROC_SERVER)?;
        tasks.AddObject(&link)?;
        list.AddUserTasks(&tasks.cast::<IObjectArray>()?)?;
        list.CommitList()?;
    }
    Ok(())
}
