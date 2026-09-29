/*
 * Imperative tunnel actions shared by the manager tab, menus, commands and dialogs. Errors are reported as toasts;
 * a locked vault (HTTP 423) is handled by the API client (unlock dialog, then retry).
 */
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { confirm } from '@/components/ui/dialog-host'
import { copyText, errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { openTab } from '@/stores/workspace'
import {
  addSessionForward,
  createTunnel,
  deleteTunnel,
  dropTunnel,
  duplicateTunnel,
  exportTunnels,
  putTunnel,
  removeSessionForward,
  restartTunnel,
  startAllTunnels,
  startTunnel,
  stopAllTunnels,
  stopTunnel,
  updateTunnel,
} from './api'
import { browserOnNexTermHost, browserReachableHost, browserUrl, copyTarget, dockerHost, hostPort, isActive, kindOf, webEndpoint, webScheme } from './model'
import { tunnelsSettings } from './settings'
import type { RemotePort, SessionForward, TunnelEx } from './types'

function fail(title: string, err: unknown): void {
  if (isApiError(err) && err.status === 423) return // the unlock dialog was cancelled
  toast.error(title, { description: errorMessage(err) })
}

export function openTunnelsTab(): void {
  openTab({ kind: 'tunnels' })
}

export async function startTunnelAction(t: TunnelEx): Promise<TunnelEx | null> {
  try {
    const next = await startTunnel(t.id)
    putTunnel(next)
    return next
  } catch (err) {
    fail(`Could not start “${t.name}”`, err)
    return null
  }
}

export async function stopTunnelAction(t: TunnelEx): Promise<void> {
  try {
    putTunnel(await stopTunnel(t.id))
  } catch (err) {
    fail(`Could not stop “${t.name}”`, err)
  }
}

export async function toggleTunnelAction(t: TunnelEx): Promise<void> {
  if (isActive(t.status)) await stopTunnelAction(t)
  else await startTunnelAction(t)
}

export async function restartTunnelAction(t: TunnelEx): Promise<void> {
  try {
    putTunnel(await restartTunnel(t.id))
  } catch (err) {
    fail(`Could not restart “${t.name}”`, err)
  }
}

export async function deleteTunnelAction(t: TunnelEx): Promise<boolean> {
  if (tunnelsSettings.get().confirmDelete) {
    const ok = await confirm({
      title: `Delete “${t.name}”?`,
      description: isActive(t.status) ? 'The tunnel is running and will be stopped.' : 'The tunnel definition is removed.',
      confirmLabel: 'Delete',
      destructive: true,
    })
    if (!ok) return false
  }
  try {
    await deleteTunnel(t.id)
    dropTunnel(t.id)
    return true
  } catch (err) {
    fail(`Could not delete “${t.name}”`, err)
    return false
  }
}

export async function duplicateTunnelAction(t: TunnelEx): Promise<TunnelEx | null> {
  try {
    const copy = await duplicateTunnel(t.id)
    putTunnel(copy)
    return copy
  } catch (err) {
    fail(`Could not duplicate “${t.name}”`, err)
    return null
  }
}

export async function setAutoStart(t: TunnelEx, autoStart: boolean): Promise<void> {
  putTunnel({ ...t, autoStart })
  try {
    putTunnel(await updateTunnel(t.id, { autoStart }))
  } catch (err) {
    putTunnel(t)
    fail('Could not change autostart', err)
  }
}

export async function startAllAction(ids?: string[]): Promise<void> {
  try {
    const res = await startAllTunnels(ids)
    if (res.failed.length) {
      toast.error(`${res.failed.length} tunnel${res.failed.length === 1 ? '' : 's'} could not start`, {
        description: res.failed
          .slice(0, 4)
          .map((f) => `${f.name}: ${f.error}`)
          .join('\n'),
        duration: 8000,
      })
    } else if (res.started) {
      toast.success(`Started ${res.started} tunnel${res.started === 1 ? '' : 's'}`)
    } else {
      toast.info('All tunnels are already running')
    }
  } catch (err) {
    fail('Could not start the tunnels', err)
  }
}

export async function stopAllAction(ids?: string[]): Promise<void> {
  try {
    const res = await stopAllTunnels(ids)
    toast.success(res.stopped ? `Stopped ${res.stopped} tunnel${res.stopped === 1 ? '' : 's'}` : 'No tunnel was running')
  } catch (err) {
    fail('Could not stop the tunnels', err)
  }
}

export async function copyTunnelTarget(t: TunnelEx): Promise<void> {
  const text = copyTarget(t)
  if (!text) {
    toast.info('Start the tunnel first: its port is assigned when it starts')
    return
  }
  if (await copyText(text)) toast.success('Copied', { description: text })
  else toast.error('Could not copy to the clipboard')
}

/** Copy "DOCKER_HOST=…" of a tunnel to a Docker socket (TUN-6). */
export async function copyDockerHostAction(t: TunnelEx): Promise<void> {
  const value = dockerHost(t)?.value
  if (!value) {
    toast.info('Start the tunnel first: its port is assigned when it starts')
    return
  }
  const text = `DOCKER_HOST=${value}`
  if (await copyText(text)) toast.success('Copied', { description: text })
  else toast.error('Could not copy to the clipboard')
}

/** Whether "Open in browser" makes sense for a tunnel. */
export function canOpenInBrowser(t: TunnelEx): boolean {
  return kindOf(t) === 'local' && !t.options.bindSocket && !t.options.destSocket && !!webScheme(t.destPort, t.options.scheme)
}

function webProxyAvailable(): boolean {
  return isCommandEnabled('webproxy.open')
}

/**
 * Whether web services should open through the NexTerm web proxy: it is registered, and preferred — or needed
 * because this browser does not run on the NexTerm host (server mode, remote access), where tunnel listeners live.
 */
function preferWebProxy(): boolean {
  if (!webProxyAvailable()) return false
  const pref = tunnelsSettings.get().openWith
  const server = useAuthStore.getState().state?.mode === 'server'
  return pref === 'proxy' || (pref === 'auto' && (server || !browserOnNexTermHost()))
}

function openViaProxy(t: TunnelEx): void {
  void runCommand(
    'webproxy.open',
    { tunnelId: t.id, connectionId: t.connectionId, host: t.destHost, port: t.destPort, scheme: webScheme(t.destPort, t.options.scheme) ?? 'http' },
    { source: 'api' },
  )
}

/** Explain that a listener on the NexTerm host is loopback-only and this browser runs elsewhere. */
function unreachableToast(addr: string): void {
  toast.info('This browser cannot reach the tunnel', {
    description: `It listens on ${addr} of the NexTerm host, which is another machine. ${
      webProxyAvailable() ? 'Open it through the NexTerm web proxy, or listen' : 'Listen'
    } on an address this browser can reach.`,
    duration: 10_000,
  })
}

/**
 * Open a tunnel's web service: through the NexTerm web proxy when preferred / needed and available, else directly at
 * the tunnel's address as this browser reaches it (starting the tunnel first when needed).
 */
export async function openTunnelInBrowser(t: TunnelEx): Promise<void> {
  if (preferWebProxy()) {
    openViaProxy(t)
    return
  }
  if (isActive(t.status)) {
    const url = browserUrl(t)
    if (url) window.open(url, '_blank', 'noopener,noreferrer')
    else {
      const ep = webEndpoint(t)
      if (ep) unreachableToast(hostPort(ep.host, ep.port))
      else toast.info('The tunnel’s address is not known yet — try again in a moment')
    }
    return
  }
  // Start it first; open the window now (inside the click) so the popup blocker lets it through.
  const win = window.open('', '_blank')
  const started = await startTunnelAction(t)
  const target = started ? browserUrl(started) : null
  if (!target) {
    win?.close()
    const ep = started ? webEndpoint(started) : null
    if (ep) unreachableToast(hostPort(ep.host, ep.port))
    else if (started) toast.info('The tunnel started, but its address is not known yet — try again in a moment')
    return
  }
  if (win) {
    win.opener = null
    win.location.href = target
  } else {
    window.open(target, '_blank', 'noopener,noreferrer')
  }
}

export async function exportTunnelsAction(): Promise<void> {
  try {
    const file = await exportTunnels()
    const blob = new Blob([JSON.stringify(file, null, 2)], { type: 'application/json' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `nexterm-tunnels-${new Date().toISOString().slice(0, 10)}.json`
    document.body.appendChild(a)
    a.click()
    a.remove()
    setTimeout(() => URL.revokeObjectURL(a.href), 10_000)
    toast.success(`Exported ${file.tunnels.length} tunnel${file.tunnels.length === 1 ? '' : 's'}`, {
      description: 'Passwords are not included.',
    })
  } catch (err) {
    fail('Could not export the tunnels', err)
  }
}

// --- remote ports (TUN-9) --------------------------------------------------------------------------------------------

function isConflict(err: unknown): boolean {
  return isApiError(err) && err.status === 409
}

/** Local port to try first for a forwarded remote port: the same number when unprivileged. */
function preferredPort(p: RemotePort): number {
  return p.port >= 1024 ? p.port : 0
}

/**
 * Forward a remote listening port to this machine: as a temporary session forward when a session is given (it lives
 * with the session), else as a saved local tunnel that is started immediately. Returns the local address.
 */
export async function forwardRemotePort(
  p: RemotePort,
  target: { sessionId?: string; connectionId?: string },
  opts: { open?: boolean } = {},
): Promise<string | null> {
  // The forward listens on the NexTerm host (loopback). A browser elsewhere opens web services through the web
  // proxy instead (no forward needed), or cannot open them at all.
  if (opts.open && preferWebProxy()) {
    openRemotePortViaProxy(p, target)
    return null
  }
  const openHere = !!opts.open && browserOnNexTermHost()
  // Keep the popup inside the user gesture.
  const win = openHere ? window.open('', '_blank') : null
  let local: string | null = null
  try {
    if (target.sessionId) {
      const spec = { type: 'local' as const, bindHost: '127.0.0.1', destHost: p.connectHost, destPort: p.port }
      let f: SessionForward
      try {
        f = await addSessionForward(target.sessionId, { ...spec, bindPort: preferredPort(p) })
      } catch (err) {
        if (!isConflict(err) || preferredPort(p) === 0) throw err
        f = await addSessionForward(target.sessionId, { ...spec, bindPort: 0 })
      }
      if (f.status.state === 'error') throw new Error(f.status.error || 'the forward could not start')
      local = f.status.localAddr ?? null
    } else if (target.connectionId) {
      const name = `${p.process ? `${p.process} ` : ''}:${p.port}`
      const t = await createTunnel({
        name,
        type: 'local',
        connectionId: target.connectionId,
        bindHost: '127.0.0.1',
        bindPort: preferredPort(p),
        destHost: p.connectHost,
        destPort: p.port,
        autoStart: false,
        options: {},
      })
      putTunnel(t)
      let started: TunnelEx
      try {
        started = await startTunnel(t.id)
      } catch (err) {
        if (!isConflict(err) || preferredPort(p) === 0) throw err
        putTunnel(await updateTunnel(t.id, { bindPort: 0 }))
        started = await startTunnel(t.id)
      }
      putTunnel(started)
      local = started.status.localAddr ?? null
    }
  } catch (err) {
    win?.close()
    fail(`Could not forward port ${p.port}`, err)
    return null
  }
  if (!local) {
    win?.close()
    return null
  }
  const hp = /^(.*):(\d+)$/.exec(local)
  const host = hp ? browserReachableHost(hp[1].replace(/^\[|\]$/g, '')) : null
  if (opts.open && !host) {
    toast.success(`Port ${p.port} forwarded`, { description: `Listening on ${local} of the NexTerm host — this browser runs on another machine and cannot open it.` })
  } else {
    toast.success(`Port ${p.port} forwarded`, { description: `Reachable at ${local}` })
  }
  if (win) {
    if (hp && host) {
      win.opener = null
      win.location.href = `${webScheme(p.port) ?? 'http'}://${hostPort(host, hp[2])}/`
    } else {
      win.close()
    }
  }
  return local
}

/** Open a remote port's web service through the NexTerm web proxy (it dials through the session / connection). */
export function openRemotePortViaProxy(p: RemotePort, target: { sessionId?: string; connectionId?: string }): void {
  void runCommand(
    'webproxy.open',
    { ...(target.sessionId ? { sessionId: target.sessionId } : { connectionId: target.connectionId }), host: p.connectHost, port: p.port, scheme: webScheme(p.port) ?? 'http' },
    { source: 'api' },
  )
}

export async function removeSessionForwardAction(f: SessionForward): Promise<void> {
  try {
    await removeSessionForward(f.id)
  } catch (err) {
    fail('Could not stop the forward', err)
  }
}
