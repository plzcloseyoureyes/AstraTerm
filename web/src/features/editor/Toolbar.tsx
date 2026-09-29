/* Editor tab toolbar (text, hex and merge modes). Buttons appear only for the actions a tab provides. */
import type { ReactNode } from 'react'
import {
  Binary,
  Download,
  Ellipsis,
  FileDiff,
  FileOutput,
  FileText,
  Link2,
  Lock,
  Map as MapIcon,
  Command,
  WandSparkles,
  LockOpen,
  Redo2,
  RefreshCw,
  Replace,
  Save,
  SaveAll,
  Search,
  TextCursorInput,
  Pilcrow,
  Printer,
  Undo2,
  WrapText,
  ZoomIn,
  ZoomOut,
} from 'lucide-react'
import { useKeybindings } from '@/app/commands'
import type { IconType } from '@/app/registry'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { IconButton } from '@/components/ui/icon-button'
import { Spinner } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, isMac } from '@/lib/utils'

function Btn({ icon, label, command, onClick, active, disabled, className }: {
  icon: IconType
  label: string
  command?: string
  onClick: () => void
  active?: boolean
  disabled?: boolean
  className?: string
}) {
  const keys = useKeybindings(command)
  return (
    <IconButton
      icon={icon}
      label={label}
      shortcut={keys[0]}
      size="sm"
      active={active}
      disabled={disabled}
      className={cn('size-7', className)}
      onMouseDown={(e) => e.preventDefault() /* keep focus in the editor */}
      onClick={onClick}
    />
  )
}

function Sep({ className }: { className?: string }) {
  return <div className={cn('mx-1 h-4 w-px shrink-0 bg-border', className)} aria-hidden />
}

export interface MoreItem {
  label: string
  icon?: IconType
  onSelect: () => void
  disabled?: boolean
  separatorBefore?: boolean
  shortcut?: string
}

export interface EditorToolbarProps {
  mode: 'text' | 'hex'
  dirty: boolean
  saving: boolean
  canSave: boolean
  onSave: () => void
  onSaveAs?: () => void
  canUndo?: boolean
  canRedo?: boolean
  onUndo?: () => void
  onRedo?: () => void
  onFind?: () => void
  onReplace?: () => void
  onGoto?: () => void
  wrap?: boolean
  whitespace?: boolean
  /** Minimap state (undefined: no minimap toggle). */
  minimap?: boolean
  vim?: boolean
  onToggle?: (what: 'wrap' | 'whitespace' | 'minimap' | 'vim') => void
  /** Format the document (languages with a formatter). */
  onFormat?: () => void
  /** Monaco's own command palette (F1). */
  onCommandPalette?: () => void
  onZoom?: (delta: number) => void
  readOnly: boolean
  /** Undefined = read-only cannot be changed (e.g. truncated file). */
  onToggleReadOnly?: () => void
  onReload?: () => void
  onCompare?: () => void
  onSwitchMode?: () => void
  onDownload?: () => void
  onCopyPath?: () => void
  /** HTML export / printing with syntax colours (text view). */
  onExportHtml?: () => void
  onPrint?: () => void
  more?: MoreItem[]
  /** Right-aligned extra content (e.g. hex INS toggle). */
  children?: ReactNode
}

export function EditorToolbar(p: EditorToolbarProps) {
  const saveKeys = useKeybindings('editor.save')
  // A save swaps the icon for a spinner only when it takes a while; a quick one changes nothing in the toolbar (no
  // shrinking button, no dimming, no buttons jumping sideways).
  const savingShown = useDelayedFlag(p.saving)
  const moreItems: MoreItem[] = []
  if (p.onFormat) moreItems.push({ label: 'Format document', icon: WandSparkles, onSelect: p.onFormat, shortcut: isMac ? '⇧⌥F' : 'Shift+Alt+F' })
  if (p.onCommandPalette) moreItems.push({ label: 'Editor commands…', icon: Command, onSelect: p.onCommandPalette, shortcut: 'F1' })
  moreItems.push(...(p.more ?? []))
  if (p.onSwitchMode)
    moreItems.push({ label: p.mode === 'hex' ? 'Open as text' : 'Open in hex editor', icon: p.mode === 'hex' ? FileText : Binary, onSelect: p.onSwitchMode })
  if (p.onCopyPath) moreItems.push({ label: 'Copy path', icon: Link2, onSelect: p.onCopyPath })
  if (p.onDownload) moreItems.push({ label: 'Download', icon: Download, onSelect: p.onDownload })
  if (p.onExportHtml) moreItems.push({ label: 'Export as HTML…', icon: FileOutput, onSelect: p.onExportHtml, separatorBefore: true })
  if (p.onPrint) moreItems.push({ label: 'Print…', icon: Printer, onSelect: p.onPrint, separatorBefore: !p.onExportHtml })
  // Two groups: the left one gives way (clips) on very narrow panes, the right one (read-only, More…) never does.
  return (
    <div role="toolbar" aria-label="Editor" className="flex h-9 shrink-0 items-center gap-0.5 border-b bg-toolbar px-1.5">
      <div className="flex min-w-0 flex-1 items-center gap-0.5 overflow-hidden">
        <Tooltip content={p.readOnly ? 'Read-only' : p.dirty ? 'Save changes' : 'No unsaved changes'} shortcut={saveKeys[0]}>
          <Button
            size="sm"
            variant={p.dirty && !p.readOnly ? 'default' : 'ghost'}
            className="h-7 gap-1 px-2"
            disabled={!p.canSave || p.readOnly}
            aria-busy={p.saving || undefined}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => !p.saving && p.onSave()}
            aria-label="Save"
          >
            {savingShown ? <Spinner immediate className="size-3.5" label="Saving" /> : <Save className="size-3.5" />}
            <span className="hidden @md:inline">Save</span>
          </Button>
        </Tooltip>
        {p.onSaveAs && <Btn icon={SaveAll} label="Save as…" command="editor.saveAs" onClick={p.onSaveAs} />}
        {(p.onUndo || p.onRedo) && <Sep />}
        {p.onUndo && <Btn icon={Undo2} label="Undo" onClick={p.onUndo} disabled={!p.canUndo || p.readOnly} />}
        {p.onRedo && <Btn icon={Redo2} label="Redo" onClick={p.onRedo} disabled={!p.canRedo || p.readOnly} />}
        {(p.onFind || p.onGoto) && <Sep />}
        {p.onFind && <Btn icon={Search} label="Find" command="editor.find" onClick={p.onFind} />}
        {p.onReplace && <Btn icon={Replace} label="Find and replace (regex)" command="editor.replace" onClick={p.onReplace} disabled={p.readOnly} className="hidden @sm:inline-flex" />}
        {p.onGoto && (
          <Btn icon={TextCursorInput} label={p.mode === 'hex' ? 'Go to offset' : 'Go to line'} command="editor.gotoLine" onClick={p.onGoto} />
        )}
        {p.onToggle && p.mode === 'text' && (
          <>
            <Sep className="hidden @lg:block" />
            <Btn icon={WrapText} label="Word wrap" command="editor.toggleWordWrap" active={!!p.wrap} onClick={() => p.onToggle?.('wrap')} className="hidden @lg:inline-flex" />
            <Btn icon={Pilcrow} label="Show whitespace" command="editor.toggleWhitespace" active={!!p.whitespace} onClick={() => p.onToggle?.('whitespace')} className="hidden @lg:inline-flex" />
            {p.minimap !== undefined && (
              <Btn icon={MapIcon} label="Minimap" command="editor.toggleMinimap" active={p.minimap} onClick={() => p.onToggle?.('minimap')} className="hidden @xl:inline-flex" />
            )}
            <Tooltip content="Vim mode">
              <Button
                size="sm"
                variant="ghost"
                aria-pressed={!!p.vim}
                aria-label="Vim mode"
                className={cn('hidden h-7 px-1.5 font-mono text-xs text-muted-foreground @lg:inline-flex', p.vim && 'bg-accent text-foreground')}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => p.onToggle?.('vim')}
              >
                VIM
              </Button>
            </Tooltip>
          </>
        )}
        {p.onZoom && (
          <>
            <Sep className="hidden @xl:block" />
            <Btn icon={ZoomOut} label="Zoom out" command="editor.zoomOut" onClick={() => p.onZoom?.(-1)} className="hidden @xl:inline-flex" />
            <Btn icon={ZoomIn} label="Zoom in" command="editor.zoomIn" onClick={() => p.onZoom?.(1)} className="hidden @xl:inline-flex" />
          </>
        )}
        <div className="min-w-2 flex-1" />
        {p.children}
      </div>
      <div className="flex shrink-0 items-center gap-0.5">
        {p.onCompare && <Btn icon={FileDiff} label="Compare with saved version" command="editor.compareWithSaved" onClick={p.onCompare} className="hidden @sm:inline-flex" />}
        {p.onReload && <Btn icon={RefreshCw} label="Reload from disk" command="editor.reload" onClick={p.onReload} />}
        <Btn
          icon={p.readOnly ? Lock : LockOpen}
          label={p.onToggleReadOnly ? (p.readOnly ? 'Read-only — click to allow editing' : 'Make read-only') : 'Read-only'}
          onClick={() => p.onToggleReadOnly?.()}
          active={p.readOnly}
          disabled={!p.onToggleReadOnly}
        />
        {moreItems.length > 0 && (
          <DropdownMenu>
            <Tooltip content="More actions">
              <DropdownMenuTrigger asChild>
                <Button size="icon-sm" variant="ghost" className="size-7 text-muted-foreground hover:text-foreground" aria-label="More actions">
                  <Ellipsis className="size-4" />
                </Button>
              </DropdownMenuTrigger>
            </Tooltip>
            <DropdownMenuContent align="end" className="min-w-52">
              {moreItems.map((m, i) => (
                <div key={`${m.label}-${i}`}>
                  {m.separatorBefore && i > 0 && <DropdownMenuSeparator />}
                  <DropdownMenuItem disabled={m.disabled} onSelect={m.onSelect}>
                    {m.icon ? <m.icon /> : <span className="size-3.5" />}
                    {m.label}
                    {m.shortcut && <DropdownMenuShortcut>{m.shortcut}</DropdownMenuShortcut>}
                  </DropdownMenuItem>
                </div>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </div>
    </div>
  )
}
