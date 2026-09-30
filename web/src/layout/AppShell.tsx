import { useEffect } from 'react'
import './builtins'
import { installKeybindings } from '@/app/keybindings'
import { DialogHost } from '@/components/ui/dialog-host'
import { TITLE_BAR } from '@/lib/desktop'
import { useIsMobile } from '@/lib/hooks'
import { cn } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { useUIStore } from '@/stores/ui'
import { getPopoutWindows, onWorkspaceWindow, setTitleBarTabs, useActiveTab } from '@/stores/workspace'
import { AboutDialog } from './AboutDialog'
import { CommandPalette } from './CommandPalette'
import { LockScreen, useAutoLock } from './LockScreen'
import { MenuBar } from './MenuBar'
import { OverlayHost } from './OverlayHost'
import { PromptHost } from './PromptHost'
import { Ribbon } from './Ribbon'
import { Sidebar } from './Sidebar'
import { StatusBar } from './StatusBar'
import { VaultUnlockDialog } from './VaultUnlockDialog'
import { WindowControls } from './WindowControls'
import { Workspace } from './workspace/Workspace'

/**
 * The shell is a fixed-size application: its document must never scroll. body has overflow:hidden, so the user cannot
 * scroll back if a programmatic scroll (focus()/scrollIntoView() on an element briefly outside the viewport) ever
 * shifts the whole UI — snap it back. Layout keeps the document from overflowing in the first place (index.css).
 */
function pinDocumentScroll(win: Window): () => void {
  const onScroll = () => {
    if (win.scrollX !== 0 || win.scrollY !== 0) win.scrollTo(0, 0)
  }
  win.addEventListener('scroll', onScroll, { passive: true })
  onScroll()
  return () => win.removeEventListener('scroll', onScroll)
}

/**
 * Application shell: menu bar, ribbon, sidebar, dock workspace, status bar, plus global overlays (palette, prompts,
 * vault unlock, feature overlays from registerOverlay, lock screen) and the keyboard shortcut dispatcher.
 */
export function AppShell() {
  const a = appearanceSettings.use()
  const locked = useUIStore((s) => s.locked)
  const mobile = useIsMobile()
  // Desktop: tabs live in the title bar (groups hide their own headers); with the title bar hidden, or on phones, each
  // group shows its tabs itself.
  const titleBarTabs = a.showMenuBar && !mobile
  useEffect(() => setTitleBarTabs(titleBarTabs), [titleBarTabs])
  useAutoLock()

  useEffect(() => {
    const offMain = installKeybindings(window)
    const offScroll = pinDocumentScroll(window)
    const offWin = onWorkspaceWindow((w) => {
      installKeybindings(w)
      pinDocumentScroll(w)
    })
    return () => {
      offMain()
      offScroll()
      offWin()
    }
  }, [])

  // Browser tab title follows the active dock tab.
  const activeTitle = useActiveTab()?.title
  useEffect(() => {
    document.title = activeTitle ? `${activeTitle} — AstraTerm` : 'AstraTerm'
  }, [activeTitle])
  useEffect(() => () => void (document.title = 'AstraTerm'), [])

  // Pop-out windows are outside this document: hide their content while the screen is locked (SEC-5).
  useEffect(() => {
    const wins = getPopoutWindows()
    for (const w of wins) {
      try {
        const body = w.document.body
        body.toggleAttribute('inert', locked)
        body.style.filter = locked ? 'blur(18px)' : ''
      } catch {
        /* window closed */
      }
    }
  }, [locked])

  return (
    <>
      <div
        className={cn('flex h-full w-full flex-col bg-sidebar transition-[filter] duration-200', locked && 'pointer-events-none blur-xl select-none')}
        inert={locked}
        aria-hidden={locked || undefined}
      >
        {(a.showMenuBar || mobile) && <MenuBar tabs={titleBarTabs} />}
        {/* Title bar hidden in the desktop app: keep a strip for the window buttons and dragging. */}
        {TITLE_BAR && !a.showMenuBar && (
          <div className="flex h-10 shrink-0 justify-end" data-tauri-drag-region="deep">
            {TITLE_BAR === 'windows' && <WindowControls />}
          </div>
        )}
        {a.showRibbon && <Ribbon />}
        <div className="flex min-h-0 flex-1">
          {(a.showSidebar || mobile) && <Sidebar />}
          {/* Desktop: the workspace floats as one rounded card on the chrome surface — the only edge in the shell. */}
          <main
            className={cn(
              'relative min-w-0 flex-1 overflow-clip bg-panel',
              !mobile && 'mr-1.5 rounded-lg shadow-xs ring-1 ring-border/60',
              !mobile && !a.showSidebar && 'ml-1.5',
              !mobile && !a.showStatusBar && 'mb-1.5',
              !mobile && !(a.showMenuBar || a.showRibbon || TITLE_BAR) && 'mt-1.5',
            )}
            aria-label="Workspace"
          >
            <Workspace />
          </main>
        </div>
        {a.showStatusBar && <StatusBar />}
      </div>
      {!locked && (
        <>
          <CommandPalette />
          <PromptHost />
          <VaultUnlockDialog />
          <DialogHost />
          <AboutDialog />
        </>
      )}
      <OverlayHost />
      <LockScreen />
    </>
  )
}
