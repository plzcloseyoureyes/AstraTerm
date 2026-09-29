/*
 * Settings → Recordings & sharing: the user's player and share defaults (the admin policy lives in the Recordings tab's
 * "Storage & retention" page).
 */
import { ExternalLink } from 'lucide-react'
import { runCommand } from '@/app/commands'
import { Button } from '@/components/ui/button'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { SwitchField } from '@/components/ui/switch'
import { recordingsSettings } from './settings'

function Row({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <div className="text-base">{label}</div>
        {hint && <div className="text-sm text-muted-foreground">{hint}</div>}
      </div>
      {children}
    </div>
  )
}

export default function RecordingsSettingsSection() {
  const s = recordingsSettings.use()
  return (
    <div className="grid max-w-2xl gap-6">
      <section className="grid gap-3">
        <h3 className="text-md font-semibold">Player</h3>
        <Row label="Default speed">
          <SimpleSelect
            size="sm"
            className="w-24"
            value={String(s.speed)}
            onValueChange={(v) => recordingsSettings.set({ speed: Number(v) })}
            options={['0.5', '1', '1.5', '2', '4', '8'].map((v) => ({ value: v, label: `${v}×` }))}
          />
        </Row>
        <Row label="Shorten pauses to" hint="Long idle periods play faster; 0 keeps them as recorded.">
          <NumberInput inputSize="sm" className="w-28" value={s.idleTimeLimit} min={0} max={60} step={0.5} integer={false} unit="s" onChange={(v) => recordingsSettings.set({ idleTimeLimit: v ?? 0 })} />
        </Row>
        <SwitchField label="Start playing when a recording opens" checked={s.autoPlay} onCheckedChange={(v) => recordingsSettings.set({ autoPlay: v })} />
        <SwitchField label="Pause at every command marker" checked={s.pauseOnMarkers} onCheckedChange={(v) => recordingsSettings.set({ pauseOnMarkers: v })} />
        <Row label="Instant replay covers the last" hint="Offered first in the terminal menu.">
          <NumberInput inputSize="sm" className="w-28" value={s.replayMinutes} min={1} max={1440} unit="min" onChange={(v) => recordingsSettings.set({ replayMinutes: v ?? 5 })} />
        </Row>
      </section>
      <section className="grid gap-3">
        <h3 className="text-md font-semibold">Session sharing</h3>
        <Row label="New links are">
          <SimpleSelect
            size="sm"
            className="w-36"
            value={s.shareMode}
            onValueChange={(v) => recordingsSettings.set({ shareMode: v })}
            options={[
              { value: 'read', label: 'View only' },
              { value: 'write', label: 'Interactive' },
            ]}
          />
        </Row>
        <SwitchField label="Require viewers to sign in by default" checked={s.shareRequireLogin} onCheckedChange={(v) => recordingsSettings.set({ shareRequireLogin: v })} />
      </section>
      <div>
        <Button size="sm" variant="secondary" onClick={() => void runCommand('recordings.open', { tab: 'storage' })}>
          <ExternalLink /> Storage, retention & policy
        </Button>
      </div>
    </div>
  )
}
