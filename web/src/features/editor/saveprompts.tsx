/* The questions a save of a remote file can raise (EditorTab → save): conflict, permission denied, unencodable text. */
import { FilePlus2, FileUp, GitCompareArrows, RotateCcw, ShieldCheck } from 'lucide-react'
import { formatDateTime } from '@/lib/utils'
import { choose } from './dialogs'

/** The file changed on the server since it was loaded (409): what now? */
export function askConflict(name: string, serverMtime?: string): Promise<'cancel' | 'reload' | 'overwrite' | 'diff'> {
  return choose({
    title: `${name} was changed on the server`,
    description: `It changed${serverMtime ? ` (${formatDateTime(serverMtime)})` : ''} after you opened it. Your changes are not saved yet.`,
    tone: 'warning',
    choices: [
      { value: 'diff', label: 'Compare & merge…', hint: 'See both versions side by side and keep what you need from each.', icon: GitCompareArrows, variant: 'default' },
      { value: 'reload', label: 'Reload from the server', hint: 'Load the version on the server and discard your changes.', icon: RotateCcw },
      { value: 'overwrite', label: 'Overwrite', hint: 'Save your version over the one on the server.', icon: FileUp, variant: 'destructive' },
      { value: 'cancel', label: 'Cancel' },
    ],
    cancel: 'cancel',
    focus: 'diff',
  })
}

/** The server refused the write: save through sudo (when the file system can), save elsewhere, or give up. */
export function askPermissionDenied(opts: { path: string; host?: string; message: string; canSudo: boolean }): Promise<'cancel' | 'saveas' | 'sudo'> {
  return choose<'cancel' | 'saveas' | 'sudo'>({
    title: 'Permission denied',
    description: (
      <>
        You don&apos;t have permission to write <span className="font-mono text-foreground">{opts.path}</span>
        {opts.host ? ` on ${opts.host}` : ''}.
      </>
    ),
    details: <p className="rounded-md border bg-muted/50 px-2.5 py-1.5 font-mono text-xs text-muted-foreground">{opts.message}</p>,
    tone: 'security',
    choices: opts.canSudo
      ? [
          { value: 'sudo', label: 'Save with sudo', hint: 'Write it as root. NexTerm asks for the sudo password when the connection has none saved.', icon: ShieldCheck, variant: 'default' },
          { value: 'saveas', label: 'Save as…', hint: 'Keep your changes in another file you can write.', icon: FilePlus2 },
          { value: 'cancel', label: 'Cancel' },
        ]
      : [
          { value: 'cancel', label: 'Cancel' },
          { value: 'saveas', label: 'Save as…', variant: 'default' },
        ],
    cancel: 'cancel',
  })
}

/** The text has characters the document's encoding cannot store: convert to UTF-8? */
export async function askSaveAsUtf8(encodingLabel: string, message: string): Promise<boolean> {
  const r = await choose({
    title: `Cannot save as ${encodingLabel}`,
    description: message,
    tone: 'warning',
    choices: [
      { value: 'cancel', label: 'Cancel' },
      { value: 'utf8', label: 'Save as UTF-8', variant: 'default' },
    ],
    cancel: 'cancel',
  })
  return r === 'utf8'
}
