/*
 * Renders the session editor and folder dialog. The shell has no always-mounted slot for feature dialogs (sidebar
 * panels, status items and tabs can all be hidden), so the dialogs live in their own small React root that is created
 * on first use and shares the app's query client and stores.
 *
 * While the screen is locked the dialogs are hidden but stay mounted, so unsaved input survives the lock; they are
 * dropped on sign-out. App shortcuts are suspended while a dialog is visible (it is modal).
 */
import { StrictMode, Suspense, lazy, useEffect } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { Bug } from 'lucide-react'
import { suspendKeybindings } from '@/app/keybindings'
import { queryClient } from '@/api/queryClient'
import { ErrorBoundary } from '@/components/error-boundary'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { TooltipProvider } from '@/components/ui/tooltip'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { closeAllSessionDialogs, useSessionDialogs } from './store'

const SessionEditorDialog = lazy(() => import('../editor/SessionEditorDialog'))
const FolderDialog = lazy(() => import('../FolderDialog'))

function DialogCrash({ error, onClose }: { error: Error; onClose: () => void }) {
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="sm">
        <DialogHeader>
          <DialogTitle>
            <Bug className="size-4 text-destructive" /> The dialog crashed
          </DialogTitle>
          <DialogDescription className="font-mono text-sm break-words">{error.message}</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button onClick={onClose}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function SessionDialogs() {
  const editor = useSessionDialogs((s) => s.editor)
  const folder = useSessionDialogs((s) => s.folder)
  const authed = useAuthStore((s) => s.status === 'authenticated')
  const locked = useUIStore((s) => s.locked)
  const visible = authed && !locked && !!(editor || folder)

  useEffect(() => {
    if (!authed) closeAllSessionDialogs()
  }, [authed])

  useEffect(() => (visible ? suspendKeybindings() : undefined), [visible])

  if (!authed) return null
  return (
    <ErrorBoundary
      label="Session dialogs"
      resetKey={`${editor?.key ?? ''}:${folder?.key ?? ''}`}
      fallback={(error, reset) => (
        <DialogCrash
          error={error}
          onClose={() => {
            closeAllSessionDialogs()
            reset()
          }}
        />
      )}
    >
      <Suspense fallback={null}>
        {editor && <SessionEditorDialog key={editor.key} request={editor} hidden={locked} />}
        {folder && <FolderDialog key={folder.key} request={folder} hidden={locked} />}
      </Suspense>
    </ErrorBoundary>
  )
}

let root: Root | null = null

function mount(): void {
  if (root || typeof document === 'undefined') return
  const el = document.createElement('div')
  el.id = 'nx-session-dialogs'
  document.body.appendChild(el)
  root = createRoot(el)
  root.render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <SessionDialogs />
        </TooltipProvider>
      </QueryClientProvider>
    </StrictMode>,
  )
}

let installed = false

/** Create the dialog root lazily, the first time a dialog is requested. */
export function installDialogHost(): void {
  if (installed) return
  installed = true
  const check = (s: { editor: unknown; folder: unknown }) => {
    if (s.editor || s.folder) mount()
  }
  useSessionDialogs.subscribe(check)
  check(useSessionDialogs.getState())
}
