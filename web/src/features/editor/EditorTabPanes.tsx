/* Full-pane states of an "editor" tab instead of the editor: the file could not be opened, is large, or is binary. */
import { Binary, CircleAlert, Download, FileQuestion, FileWarning, Folder, PlugZap, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { formatBytes } from '@/lib/utils'
import { encodingInfo } from './codec'
import { HEX_MAX, TEXT_MAX, type BinaryFile, type LoadError } from './tabstate'
import { PaneMessage } from './ui'

/** Encodings offered for decoding a file that looks binary. */
const TEXT_FALLBACKS = ['windows-1252', 'utf-8', 'iso-8859-1', 'utf-16le', 'utf-16be', 'windows-1251', 'shift_jis', 'gbk']

export function ErrorPane({
  error,
  name,
  path,
  host,
  canReconnect,
  onRetry,
  onReconnect,
  onCreate,
  onDownload,
  onClose,
}: {
  error: LoadError
  name: string
  path: string
  host?: string
  canReconnect: boolean
  onRetry: () => void
  onReconnect: () => void
  onCreate: () => void
  onDownload: () => void
  onClose: () => void
}) {
  const closeButton = (
    <Button size="sm" variant="secondary" onClick={onClose}>
      Close tab
    </Button>
  )
  if (error.tooLarge) {
    return (
      <PaneMessage
        icon={FileWarning}
        tone="warning"
        title={`${name} is too large for the editor`}
        actions={
          <>
            <Button size="sm" onClick={onDownload} autoFocus>
              <Download /> Download
            </Button>
            {closeButton}
          </>
        }
      >
        {error.message}
      </PaneMessage>
    )
  }
  if (error.isDir) {
    return (
      <PaneMessage icon={Folder} title={`${name} is a directory`} actions={closeButton}>
        <span className="font-mono">{path}</span>
      </PaneMessage>
    )
  }
  if (error.handleGone) {
    return (
      <PaneMessage
        icon={PlugZap}
        tone="warning"
        title="The file system connection is no longer available"
        actions={
          <>
            {canReconnect && (
              <Button size="sm" onClick={onReconnect} autoFocus>
                Reconnect
              </Button>
            )}
            <Button size="sm" variant="secondary" onClick={onRetry}>
              <RefreshCw /> Retry
            </Button>
            {closeButton}
          </>
        }
      >
        The session or file browser that opened <span className="font-mono">{path}</span>
        {host ? ` on ${host}` : ''} was closed, or the Termstead server restarted. {canReconnect ? '' : 'Open the file again from the file browser.'}
      </PaneMessage>
    )
  }
  if (error.notFound) {
    return (
      <PaneMessage
        icon={FileQuestion}
        title={`${name} was not found`}
        actions={
          <>
            <Button size="sm" variant="secondary" onClick={onRetry}>
              <RefreshCw /> Retry
            </Button>
            <Button size="sm" variant="secondary" onClick={onCreate}>
              Create empty file
            </Button>
            {closeButton}
          </>
        }
      >
        <span className="font-mono">{path}</span>
        {host ? ` on ${host}` : ''} does not exist (anymore).
      </PaneMessage>
    )
  }
  return (
    <PaneMessage
      icon={CircleAlert}
      tone="danger"
      title={`Could not open ${name}`}
      actions={
        <>
          <Button size="sm" onClick={onRetry} autoFocus>
            <RefreshCw /> Retry
          </Button>
          {closeButton}
        </>
      }
    >
      {error.message}
    </PaneMessage>
  )
}

/** Above settings.editor.largeFileMiB: open anyway, in the hex editor, or download. */
export function LargeFilePane({ name, size, onOpen, onHex, onDownload }: { name: string; size: number; onOpen: () => void; onHex: () => void; onDownload: () => void }) {
  return (
    <PaneMessage
      icon={FileWarning}
      tone="warning"
      title={`${name} is large (${formatBytes(size)})`}
      actions={
        <>
          {size <= TEXT_MAX && (
            <Button variant="default" size="sm" onClick={onOpen} autoFocus>
              Open anyway
            </Button>
          )}
          {size <= HEX_MAX && (
            <Button variant="secondary" size="sm" onClick={onHex}>
              <Binary /> Hex editor
            </Button>
          )}
          <Button variant={size > TEXT_MAX ? 'default' : 'secondary'} size="sm" onClick={onDownload}>
            <Download /> Download
          </Button>
        </>
      }
    >
      {size > TEXT_MAX
        ? `The editor opens files up to ${formatBytes(TEXT_MAX)}. Download the file to work with it locally.`
        : 'Opening large files in the editor can be slow and uses a lot of memory.'}
    </PaneMessage>
  )
}

/** The bytes do not look like text: hex editor, decode as text anyway, or download. */
export function BinaryPane({
  name,
  file,
  onHex,
  onDecode,
  onDownload,
}: {
  name: string
  file: BinaryFile
  onHex: () => void
  onDecode: (encoding: string) => void
  onDownload: () => void
}) {
  return (
    <PaneMessage
      icon={Binary}
      title={`${name} looks like a binary file`}
      actions={
        <>
          <Button variant="default" size="sm" autoFocus onClick={onHex}>
            <Binary /> Open in hex editor
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="secondary" size="sm">
                Open as text…
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="center" className="max-h-80 overflow-y-auto">
              <DropdownMenuLabel>Decode as</DropdownMenuLabel>
              {TEXT_FALLBACKS.map((enc) => (
                <DropdownMenuItem key={enc} onSelect={() => onDecode(enc)}>
                  {encodingInfo(enc).label}
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Button variant="secondary" size="sm" onClick={onDownload}>
            <Download /> Download
          </Button>
        </>
      }
    >
      {file.reason.replace(/\.$/, '')} ({formatBytes(file.size)}). Editing it as text could corrupt it.
    </PaneMessage>
  )
}
