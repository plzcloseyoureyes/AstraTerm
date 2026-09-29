/*
 * Wake-on-LAN (TOOL-9): magic packet to a broadcast, subnet-directed or unicast address (UDP 9 / 7 / custom), with
 * an optional SecureOn password; can be sent from a saved SSH host to wake machines on that host's subnet.
 */
import * as React from 'react'
import { useQuery } from '@tanstack/react-query'
import { Power } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { getInterfaces } from '../api'
import { EmptyResults, FormGrid, JobNotes, PanelLayout, RecentRuns, ResultArea, RunControls, Stat, StatRow, ViaConnectionField, onEnter, str } from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asString, usePrefill } from '../navigate'
import { calcSubnet } from '../subnet'
import { useToolJob } from '../useToolJob'

const DEFAULTS = { mac: '', broadcast: '255.255.255.255', port: 9, secureOn: '', count: 3, via: '' }

const MAC_RE = /^([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$|^[0-9a-f]{12}$|^([0-9a-f]{4}\.){2}[0-9a-f]{4}$/i

export default function WolPanel() {
  const job = useToolJob('wol')
  const { state } = job
  const [f, set] = useToolForm('wol', DEFAULTS)
  usePrefill('wol', (p) => set({ mac: asString(p.mac), broadcast: asString(p.broadcast, f.broadcast) }))
  const mode = useRunMode()
  const isAdmin = useIsAdmin()
  const ifaces = useQuery({ queryKey: ['tools', 'interfaces'], queryFn: getInterfaces, enabled: (mode === 'desktop' || isAdmin) && !f.via, staleTime: 60_000 })
  const broadcasts = React.useMemo(() => {
    const out: string[] = []
    for (const ifc of ifaces.data ?? []) {
      if (!ifc.up) continue
      for (const a of ifc.addrs ?? []) {
        const r = calcSubnet(a)
        if (!('error' in r) && r.family === 4 && r.prefix >= 8 && r.prefix <= 30 && !a.startsWith('127.') && !out.includes(r.broadcast)) out.push(r.broadcast)
      }
    }
    return out.slice(0, 4)
  }, [ifaces.data])

  const macOK = MAC_RE.test(f.mac.trim())
  const run = () => {
    if (!macOK || job.running) return
    const params = { mac: f.mac.trim(), broadcast: f.broadcast.trim() || undefined, port: f.port, secureOn: f.secureOn.trim() || undefined, count: f.count, viaConnectionId: f.via || undefined }
    void job.run(params)
    addRun('wol', params, f.mac.trim())
  }
  const summary = state.latest.summary

  return (
    <PanelLayout
      title="Wake-on-LAN"
      description="Send a magic packet to wake a machine (broadcast, subnet-directed or unicast), optionally from a saved SSH host on the target's network."
      form={
        <>
          <FormGrid>
            <Field label="MAC address" error={f.mac.trim() && !macOK ? 'Expected aa:bb:cc:dd:ee:ff' : undefined}>
              <Input value={f.mac} onChange={(e) => set({ mac: e.target.value })} placeholder="aa:bb:cc:dd:ee:ff" className="font-mono" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Broadcast / target address" hint={f.via ? 'IPv4 address on the SSH host’s network' : undefined}>
              <Input value={f.broadcast} onChange={(e) => set({ broadcast: e.target.value })} placeholder="255.255.255.255" className="font-mono" onKeyDown={onEnter(run)} spellCheck={false} />
            </Field>
            <Field label="UDP port" hint="9 (discard) or 7 (echo)">
              <NumberInput value={f.port} onChange={(v) => set({ port: v ?? 9 })} min={1} max={65535} />
            </Field>
            <Field label="Packets">
              <NumberInput value={f.count} onChange={(v) => set({ count: v ?? 1 })} min={1} max={20} />
            </Field>
            <Field label="SecureOn password" hint="Optional, 6 bytes (aa:bb:cc:dd:ee:ff)">
              <PasswordInput value={f.secureOn} onChange={(e) => set({ secureOn: e.target.value })} autoComplete="off" placeholder="none" />
            </Field>
            <ViaConnectionField value={f.via} onChange={(v) => set({ via: v })} label="Send from" hint="Wake a machine on a remote subnet through a saved SSH host" />
          </FormGrid>
          {broadcasts.length > 0 && (
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              <span className="text-xs text-muted-foreground">Subnet broadcasts:</span>
              {broadcasts.map((b) => (
                <Button key={b} variant="outline" size="xs" className="font-mono" onClick={() => set({ broadcast: b })}>
                  {b}
                </Button>
              ))}
            </div>
          )}
        </>
      }
      controls={
        <>
          <RunControls state={state} onRun={run} onCancel={job.cancel} onClear={job.reset} disabled={!macOK} runLabel="Wake" />
          <RecentRuns tool="wol" onPick={(r) => set({ mac: asString(r.params.mac), broadcast: asString(r.params.broadcast, '255.255.255.255'), via: asString(r.params.viaConnectionId) })} />
        </>
      }
    >
      <JobNotes state={state} />
      <ResultArea>
        {summary ? (
          <StatRow>
            <Stat label="MAC" value={<span className="font-mono text-base">{str(summary.mac)}</span>} />
            <Stat label="Sent to" value={<span className="font-mono text-base">{`${str(summary.broadcast)}:${str(summary.port)}`}</span>} />
            <Stat label="Packets" value={str(summary.packets)} tone="success" />
            <Stat label="Size" value={`${str(summary.bytes)} B`} />
            {summary.via ? <Stat label="From" value={<span className="text-base">{str(summary.via)}</span>} /> : null}
          </StatRow>
        ) : (
          <EmptyResults icon={Power} hint="Enter the MAC address of the machine to wake and press Wake." />
        )}
      </ResultArea>
    </PanelLayout>
  )
}
