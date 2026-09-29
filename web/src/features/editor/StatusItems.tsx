/* Status bar segments of the editor tabs: where a remote file is and its save state (left), the text editor's position,
 * indentation, encoding, line endings and language (right; also used by the text tab). */
import { toast } from 'sonner'
import { formatDateTime, formatRelativeTime } from '@/lib/utils'
import { encodingLabel, type Eol } from './codec'
import type { FileStat } from './filestat'
import { languageHint, languageNames } from './languages'
import type { EditorSession, SessionStatus } from './monaco/session'
import type { TextMeta } from './save'
import { EncodingMenu, EolMenu, IndentMenu, LanguagePicker, StatusButton, StatusText } from './ui'

/** "Saving…" / "Read-only" / "Unsaved changes" / "Saved 2 min ago" / "". */
export function saveStateLabel(s: { saving: boolean; readOnly: boolean; dirty: boolean; lastSaved: number | null }): string {
  if (s.saving) return 'Saving…'
  if (s.readOnly) return 'Read-only'
  if (s.dirty) return 'Unsaved changes'
  return s.lastSaved ? `Saved ${formatRelativeTime(s.lastSaved)}` : ''
}

/**
 * Left side of a remote file's status bar: the save state first, then permissions and sudo. Where the file is shows in
 * the path bar above the editor (which also copies the path), so it is not repeated here.
 */
export function FileLocation({ stat, sudo, saveState }: { stat: FileStat; sudo?: boolean; saveState: string }) {
  return (
    <>
      {saveState && (
        <StatusText className="text-foreground/80" aria-live="polite">
          {saveState}
        </StatusText>
      )}
      {stat.perm && (
        <StatusText className="hidden font-mono @lg:inline-flex" title={`${stat.owner ?? ''}${stat.group ? `:${stat.group}` : ''} · modified ${formatDateTime(stat.mtime)}`}>
          {stat.perm}
        </StatusText>
      )}
      {sudo && (
        <StatusText className="font-medium text-warning" title="Saves of this file are written through sudo">
          sudo
        </StatusText>
      )}
    </>
  )
}

let namesCache: string[] | null = null
function allLanguageNames(): string[] {
  namesCache ??= languageNames()
  return namesCache
}

/** Right side of a text editor's status bar. */
export function TextStatusItems({
  session,
  status,
  meta,
  tabSize,
  readOnly,
  onReopen,
  onSaveWith,
  onEol,
  onLanguage,
}: {
  session: EditorSession | null
  status: SessionStatus
  meta: TextMeta
  tabSize: number
  readOnly: boolean
  /** Re-decode the file with another encoding (documents read from a file). */
  onReopen?: (encoding: string) => void
  onSaveWith: (encoding: string, bom: boolean) => void
  onEol: (eol: Eol) => void
  onLanguage: (name: string) => void
}) {
  return (
    <>
      {status.large && (
        <StatusText className="text-muted-foreground" title="Large file: minimap, suggestions and background language analysis are off to keep editing fast.">
          Large file
        </StatusText>
      )}
      <StatusButton tooltip="Go to line" onClick={() => session?.gotoLine()} className="tabular">
        Ln {status.line}, Col {status.col}
      </StatusButton>
      {status.selections > 1 ? (
        <StatusText className="tabular">{status.selections} selections</StatusText>
      ) : status.selectedChars > 0 ? (
        <StatusText className="tabular">
          ({status.selectedChars} selected{status.selectedLines > 1 ? `, ${status.selectedLines} lines` : ''})
        </StatusText>
      ) : null}
      <IndentMenu
        className="hidden @md:inline-flex"
        indent={status.indent}
        tabSize={tabSize}
        onChange={(v) => session?.setIndent(v)}
        onConvert={() => session?.convertIndentation()}
        onDetect={() => {
          if (!session?.detectIndentation()) toast.info('No indentation found in this file')
        }}
        disabled={readOnly}
      />
      <EncodingMenu
        className="hidden @sm:inline-flex"
        label={encodingLabel(meta.encoding, meta.bom)}
        encoding={meta.encoding}
        bom={meta.bom}
        onReopen={onReopen}
        onSaveWith={onSaveWith}
        // Reopening with another encoding stays possible in a read-only document; re-encoding does not.
        disabled={readOnly && !onReopen}
        saveDisabled={readOnly}
      />
      <EolMenu className="hidden @sm:inline-flex" eol={meta.eol} mixed={meta.mixedEol && status.eolMarks > 0} onChange={onEol} disabled={readOnly} />
      <LanguagePicker value={status.language} loading={status.languageLoading} names={allLanguageNames()} hint={languageHint} onChange={onLanguage} />
    </>
  )
}
