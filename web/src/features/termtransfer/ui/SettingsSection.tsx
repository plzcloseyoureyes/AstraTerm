/*
 * Settings → File transfer (terminal): trzsz / ZMODEM, where downloads go, drops on a terminal, send-file pacing.
 */
import { useEffect, useState } from 'react'
import { FolderX } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { SettingRow, SettingsGroup, SettingsPage } from '@/features/settings/ui'
import { useCurrentUser } from '@/stores/auth'
import { hasFolderAccess, rememberFolder, rememberedFolder } from '../browser'
import { DEFAULT_PROMPT_PATTERN } from '../engine/pacer'
import { DEFAULTS, LIMITS, transferSettings, type ConflictPolicy, type DownloadTarget, type LineEnding, type SendMode } from '../settings'

const TARGETS: { value: DownloadTarget; label: string }[] = [
  { value: 'ask', label: 'Ask each time' },
  { value: 'folder', label: 'A folder (remembered)' },
  { value: 'downloads', label: 'Browser downloads' },
]

const CONFLICTS: { value: ConflictPolicy; label: string }[] = [
  { value: 'ask', label: 'Ask' },
  { value: 'overwrite', label: 'Replace' },
  { value: 'rename', label: 'Keep both (rename)' },
  { value: 'skip', label: 'Skip existing' },
]

const MODES: { value: SendMode; label: string }[] = [
  { value: 'text', label: 'Text, line by line' },
  { value: 'binary', label: 'Binary (raw bytes)' },
]

const EOLS: { value: LineEnding; label: string }[] = [
  { value: 'cr', label: 'Enter (CR)' },
  { value: 'crlf', label: 'CR LF' },
  { value: 'lf', label: 'LF' },
]

export default function TermTransferSettingsSection() {
  const s = transferSettings.use()
  const user = useCurrentUser()
  const folderOk = hasFolderAccess()
  const [folderName, setFolderName] = useState<string | null>(null)
  const [rz, setRz] = useState(s.rzCommand)

  useEffect(() => setRz(s.rzCommand), [s.rzCommand])
  useEffect(() => {
    if (!folderOk) return
    let live = true
    void rememberedFolder(user?.id ?? '').then((h) => live && setFolderName(h?.name ?? null))
    return () => {
      live = false
    }
  }, [folderOk, user?.id])

  return (
    <SettingsPage
      title="File transfer (terminal)"
      description="In-terminal transfers with trzsz (trz / tsz) and ZMODEM (rz / sz), files dropped on a terminal, and sending a file into a session."
      actions={
        <Button variant="secondary" size="sm" onClick={() => transferSettings.reset()}>
          Reset to defaults
        </Button>
      }
    >
      <SettingsGroup title="In-terminal transfers" description="Run trz / tsz or rz / sz on the server; NexTerm answers in the browser. Needs trzsz or lrzsz installed on the server.">
        <SettingRow label="trzsz (trz / tsz)" description="tmux-friendly, folders (trz -d / tsz -d), binary mode (-b), progress bar in the terminal." htmlFor="tt-trzsz">
          <Switch id="tt-trzsz" checked={s.trzsz} onCheckedChange={(v) => transferSettings.set({ trzsz: v })} />
        </SettingRow>
        <SettingRow label="ZMODEM (rz / sz)" description="Classic lrzsz transfers over SSH, telnet and serial sessions. Not inside tmux / screen." htmlFor="tt-zmodem">
          <Switch id="tt-zmodem" checked={s.zmodem} onCheckedChange={(v) => transferSettings.set({ zmodem: v })} />
        </SettingRow>
        <SettingRow label="Start ZMODEM downloads without asking" description="When the destination below is decided. Off: every sz asks first." htmlFor="tt-zauto">
          <Switch id="tt-zauto" checked={s.zmodemAutoReceive} disabled={!s.zmodem} onCheckedChange={(v) => transferSettings.set({ zmodemAutoReceive: v })} />
        </SettingRow>
        <SettingRow
          label="Downloaded files go to"
          description={
            folderOk
              ? 'A folder is written directly (large files stream to disk). Browser downloads keep each file in memory until complete; folders become a ZIP.'
              : 'This browser cannot write into a folder: files go through the browser’s downloads (folders as a ZIP).'
          }
        >
          <SimpleSelect
            aria-label="Downloaded files go to"
            size="sm"
            className="w-52"
            value={s.downloadTarget}
            onValueChange={(downloadTarget) => transferSettings.set({ downloadTarget })}
            options={TARGETS.map((t) => (t.value === 'folder' && !folderOk ? { ...t, disabled: true } : t))}
          />
        </SettingRow>
        {folderOk && (
          <SettingRow label="Remembered folder" description={folderName ? `Downloads are saved in “${folderName}” when a folder is chosen.` : 'None yet — chosen the first time files are received.'}>
            <Button
              variant="secondary"
              size="sm"
              disabled={!folderName}
              onClick={() => {
                void rememberFolder(user?.id ?? '', null).then(() => {
                  setFolderName(null)
                  toast.success('The download folder was forgotten')
                })
              }}
            >
              <FolderX /> Forget
            </Button>
          </SettingRow>
        )}
      </SettingsGroup>

      <SettingsGroup title="Dropping files on a terminal" description="Drag files from your computer onto a terminal and pick how they are sent.">
        <SettingRow label="When names already exist" description="For uploads into the session’s folder (SSH) or the host (local shells).">
          <SimpleSelect aria-label="When names already exist" size="sm" className="w-48" value={s.dropConflict} onValueChange={(dropConflict) => transferSettings.set({ dropConflict })} options={CONFLICTS} />
        </SettingRow>
        <SettingRow label="Type the uploaded path at the prompt (SSH)" description="Local shells always get the path typed, since the file is copied next to the shell." htmlFor="tt-typepath">
          <Switch id="tt-typepath" checked={s.dropTypePath} onCheckedChange={(v) => transferSettings.set({ dropTypePath: v })} />
        </SettingRow>
        <SettingRow label="ZMODEM upload command" description="Typed when you drop onto “Upload with rz” (-E renames instead of overwriting).">
          <Input
            aria-label="ZMODEM upload command"
            inputSize="sm"
            className="w-40 font-mono"
            value={rz}
            onChange={(e) => setRz(e.target.value)}
            onBlur={() => transferSettings.set({ rzCommand: rz.trim() || DEFAULTS.rzCommand })}
            spellCheck={false}
          />
        </SettingRow>
        <SettingRow label="“Paste contents” limit" description="Largest text file whose contents may be pasted.">
          <NumberInput
            aria-label="Paste contents limit"
            className="w-32"
            inputSize="sm"
            unit="KiB"
            value={Math.round(s.pasteMaxBytes / 1024)}
            min={LIMITS.pasteMaxBytes[0] / 1024}
            max={LIMITS.pasteMaxBytes[1] / 1024}
            onChange={(n) => transferSettings.set({ pasteMaxBytes: (n ?? 64) * 1024 })}
          />
        </SettingRow>
      </SettingsGroup>

      <SettingsGroup title="Send file to session" description="Defaults of Terminal → Send File to Session… (the dialog remembers your last choice).">
        <SettingRow label="Mode">
          <SimpleSelect aria-label="Send mode" size="sm" className="w-48" value={s.sendMode} onValueChange={(sendMode) => transferSettings.set({ sendMode })} options={MODES} />
        </SettingRow>
        <SettingRow label="Delay per line" description="Text mode. Slow devices (routers, switches, microcontrollers) may need 50–200 ms.">
          <NumberInput aria-label="Delay per line" className="w-32" inputSize="sm" unit="ms" value={s.lineDelayMs} min={LIMITS.lineDelayMs[0]} max={LIMITS.lineDelayMs[1]} step={10} onChange={(n) => transferSettings.set({ lineDelayMs: n ?? 0 })} />
        </SettingRow>
        <SettingRow label="Delay per character" description="Text mode.">
          <NumberInput aria-label="Delay per character" className="w-32" inputSize="sm" unit="ms" value={s.charDelayMs} min={LIMITS.charDelayMs[0]} max={LIMITS.charDelayMs[1]} step={5} onChange={(n) => transferSettings.set({ charDelayMs: n ?? 0 })} />
        </SettingRow>
        <SettingRow label="Line ending" description="Text mode: what is typed after each line.">
          <SimpleSelect aria-label="Line ending" size="sm" className="w-36" value={s.lineEnding} onValueChange={(lineEnding) => transferSettings.set({ lineEnding })} options={EOLS} />
        </SettingRow>
        <SettingRow label="Wait for the prompt before each next line" description={`Shell integration marks when available, else the pattern (default ${DEFAULT_PROMPT_PATTERN}).`} htmlFor="tt-wait">
          <Switch id="tt-wait" checked={s.waitPrompt} onCheckedChange={(v) => transferSettings.set({ waitPrompt: v })} />
        </SettingRow>
        <SettingRow label="Binary chunk size and delay" description="Binary mode: bytes per chunk and the pause between chunks.">
          <div className="flex items-center gap-2">
            <NumberInput aria-label="Binary chunk size" className="w-32" inputSize="sm" unit="bytes" value={s.binaryChunkBytes} min={LIMITS.binaryChunkBytes[0]} max={LIMITS.binaryChunkBytes[1]} onChange={(n) => transferSettings.set({ binaryChunkBytes: n ?? DEFAULTS.binaryChunkBytes })} />
            <NumberInput aria-label="Binary delay" className="w-28" inputSize="sm" unit="ms" value={s.binaryDelayMs} min={LIMITS.binaryDelayMs[0]} max={LIMITS.binaryDelayMs[1]} onChange={(n) => transferSettings.set({ binaryDelayMs: n ?? DEFAULTS.binaryDelayMs })} />
          </div>
        </SettingRow>
      </SettingsGroup>
    </SettingsPage>
  )
}
