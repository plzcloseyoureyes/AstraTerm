/*
 * Serial console editor (PROTO-10, CC-18). Ports are listed from GET /api/serial/ports (host ports of the AstraTerm
 * machine, refreshable after plugging a device in); any device path can still be typed. Auto-baud probes the port.
 */
import { useState } from 'react'
import { RefreshCw, Usb, Wand2 } from 'lucide-react'
import { toast } from 'sonner'
import type { ProtocolEditorProps } from '@/app/registry'
import { useSerialPorts } from '@/api/sessions'
import type { Connection } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { BusyIcon } from '@/components/ui/spinner'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { errorMessage } from '@/lib/utils'
import { autobaudSerial } from '@/features/protocols/api'
import { defineProtocol, type ValidationErrors } from './define'
import { ComboOption, EditorNote, LINE_ENDING_CHOICES, OptionSection, SelectOption, SwitchOption, optString, withOptions, type ComboSuggestion } from './fields'

export const BAUD_RATES = [110, 300, 600, 1200, 2400, 4800, 9600, 14400, 19200, 38400, 57600, 115200, 230400, 460800, 921600, 1000000, 1500000, 2000000, 3000000]

const BAUD_SUGGESTIONS: ComboSuggestion[] = BAUD_RATES.map((b) => ({ value: String(b), description: b === 9600 ? 'default' : undefined }))

export function SerialEditor({ value, onChange }: ProtocolEditorProps) {
  const ports = useSerialPorts(true)
  const mode = useRunMode()
  const isAdmin = useIsAdmin()
  const p = { value, onChange }
  const suggestions: ComboSuggestion[] = (ports.data ?? []).map((port) => ({
    value: port.name,
    description: [port.description, port.vid && port.pid ? `${port.vid}:${port.pid}` : '', port.serial ? `S/N ${port.serial}` : '']
      .filter(Boolean)
      .join(' · ') || undefined,
  }))
  return (
    <div className="grid gap-5">
      {mode === 'server' && !isAdmin && (
        <EditorNote tone="warning">In server mode, serial ports of the AstraTerm host are available to administrators only.</EditorNote>
      )}
      <OptionSection title="Port">
        <div className="grid gap-1">
          <ComboOption
            {...p}
            name="device"
            label="Serial port"
            required
            mono
            placeholder={navigator.userAgent.includes('Windows') ? 'COM3' : '/dev/ttyUSB0'}
            suggestions={suggestions}
            loading={ports.isFetching}
            onOpen={() => void ports.refetch()}
            emptyText={ports.isError ? 'Port list unavailable — type the device name' : 'No serial ports detected on the AstraTerm host'}
            hint="Ports of the machine running AstraTerm."
          />
          <Button
            type="button"
            variant="ghost"
            size="xs"
            className="justify-self-start"
            disabled={ports.isFetching}
            onClick={() => void ports.refetch()}
          >
            <BusyIcon icon={RefreshCw} busy={ports.isFetching} /> Refresh ports
          </Button>
        </div>
        <div className="grid gap-1">
          <ComboOption {...p} name="baud" label="Speed (baud)" numeric mono placeholder="9600" suggestions={BAUD_SUGGESTIONS} />
          <AutoBaudButton value={value} onChange={onChange} />
        </div>
      </OptionSection>
      <OptionSection title="Line settings">
        <SelectOption
          {...p}
          name="dataBits"
          label="Data bits"
          numeric
          defaultLabel="8 (default)"
          options={[5, 6, 7, 8].map((n) => ({ value: String(n), label: String(n) }))}
        />
        <SelectOption
          {...p}
          name="parity"
          label="Parity"
          defaultLabel="None (default)"
          options={[
            { value: 'none', label: 'None' },
            { value: 'odd', label: 'Odd' },
            { value: 'even', label: 'Even' },
            { value: 'mark', label: 'Mark' },
            { value: 'space', label: 'Space' },
          ]}
        />
        <SelectOption
          {...p}
          name="stopBits"
          label="Stop bits"
          defaultLabel="1 (default)"
          options={[
            { value: '1', label: '1' },
            { value: '1.5', label: '1.5 (Windows hosts only)' },
            { value: '2', label: '2' },
          ]}
        />
        <SelectOption
          {...p}
          name="flowControl"
          label="Flow control"
          defaultLabel="None (default)"
          options={[
            { value: 'none', label: 'None' },
            { value: 'rtscts', label: 'Hardware (RTS/CTS)' },
            { value: 'xonxoff', label: 'Software (XON/XOFF)' },
            { value: 'dsrdtr', label: 'Hardware (DSR/DTR — Windows, macOS)' },
          ]}
        />
      </OptionSection>
      <OptionSection title="Control lines" description="State of the output lines when the port opens (toggle them live from the serial toolbar).">
        <SwitchOption
          {...p}
          name="dtr"
          label="Assert DTR"
          defaultValue
          hint="Turn off for boards that reset when DTR rises (e.g. Arduino)."
        />
        <SwitchOption {...p} name="rts" label="Assert RTS" defaultValue hint="Ignored with RTS/CTS flow control." />
      </OptionSection>
      <OptionSection title="Terminal">
        <SelectOption {...p} name="lineEnding" label="Enter sends" defaultLabel="CR (default)" options={LINE_ENDING_CHOICES} />
        <div className="grid content-start gap-2 pt-1">
          <SwitchOption {...p} name="localEcho" label="Local echo" hint="Echo typed characters locally." />
          <SwitchOption {...p} name="hexView" label="Open the hex monitor" hint="Show received and sent bytes as a hex dump when the session opens." />
        </div>
      </OptionSection>
    </div>
  )
}

/** Auto-baud detection (CC-18): probes common rates on the configured port and fills the baud field with the best. */
function AutoBaudButton({ value, onChange }: { value: Connection; onChange: (next: Connection) => void }) {
  const [busy, setBusy] = useState(false)
  const [probe, setProbe] = useState(true)
  const device = optString(value, 'device').trim()
  const run = async () => {
    if (!device) {
      toast.error('Choose a serial port first')
      return
    }
    setBusy(true)
    try {
      const res = await autobaudSerial(device, { probe })
      onChange(withOptions(value, { baud: res.baud }))
      toast.success(`Detected ${res.baud} baud`, {
        description: `Confidence ${Math.round(res.score * 100)}% · sample: ${res.sample.replace(/\s+/g, ' ').trim().slice(0, 48) || '—'}`,
      })
    } catch (err) {
      toast.error('Auto-detect failed', { description: errorMessage(err) })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <Button type="button" variant="ghost" size="xs" disabled={busy || !device} onClick={run} title="Close sessions using the port first">
        <Wand2 /> {busy ? 'Detecting…' : 'Auto-detect baud'}
      </Button>
      <label className="flex items-center gap-1.5 text-sm text-muted-foreground select-none">
        <Checkbox checked={probe} onCheckedChange={(v) => setProbe(v === true)} disabled={busy} />
        Send Enter to wake the console
      </label>
    </div>
  )
}

function validateSerial(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  if (!optString(c, 'device').trim()) errors['options.device'] = 'Choose or type a serial port'
  // Imported / hand-edited options may hold a string.
  const baud: unknown = c.options?.baud
  if (baud !== undefined && baud !== null && baud !== '') {
    const n = typeof baud === 'number' ? baud : Number(baud)
    if (!Number.isInteger(n) || n < 50 || n > 20_000_000) errors['options.baud'] = 'Baud rate must be a whole number between 50 and 20000000'
  }
  return errors
}

defineProtocol({
  protocol: 'serial',
  label: 'Serial',
  icon: Usb,
  defaultPort: 0,
  group: 'terminal',
  order: 50,
  description: 'Serial console (COM / tty ports of the AstraTerm host)',
  component: SerialEditor,
  profile: { host: 'hidden', port: false, username: false, auth: 'none', kind: 'terminal', network: false },
  validate: validateSerial,
})
