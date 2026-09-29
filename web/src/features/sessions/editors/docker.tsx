/*
 * Docker editor (PROTO-30): a shell inside a container — or its log stream — of the local engine, a remote engine
 * (TCP) or an engine reached through a saved SSH connection. Containers are listed from GET /api/docker/containers
 * (also through the SSH connection).
 */
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Container } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import type { Connection } from '@/api/types'
import { useServerFeatures } from '@/stores/auth'
import { listDockerContainers } from '@/features/protocols/api'
import { defineProtocol, type ValidationErrors } from './define'
import {
  ComboOption,
  ConnectionSelectOption,
  EditorNote,
  NumberOption,
  OptionSection,
  SelectOption,
  TextOption,
  optString,
  type ComboSuggestion,
} from './fields'

export const CONTAINER_SHELLS: ComboSuggestion[] = [
  { value: '/bin/sh', description: 'default: bash when present, else sh' },
  { value: '/bin/bash' },
  { value: '/bin/ash' },
  { value: '/bin/zsh' },
  { value: 'powershell' },
  { value: 'cmd' },
]

export function DockerEditor({ value, onChange }: ProtocolEditorProps) {
  const features = useServerFeatures()
  const p = { value, onChange }
  const host = optString(value, 'dockerHost').trim()
  const via = optString(value, 'viaConnectionId')
  const logs = optString(value, 'dockerMode') === 'logs'
  const [wanted, setWanted] = useState(false)
  const containers = useQuery({
    queryKey: ['protocols', 'docker-containers', 'editor', host, via],
    placeholderData: undefined, // never show another engine's containers under this key
    queryFn: () => listDockerContainers({ host: via ? undefined : host || undefined, connectionId: via || undefined }, logs),
    enabled: wanted,
    staleTime: 10_000,
    retry: false,
  })
  const suggestions: ComboSuggestion[] = (containers.data ?? []).map((c) => ({
    value: c.name?.replace(/^\//, '') || c.id,
    description: [c.image, c.state].filter(Boolean).join(' · ') || undefined,
  }))
  return (
    <div className="grid gap-5">
      {features && features.docker === false && !host && !via && (
        <EditorNote tone="warning">No Docker engine was detected on the AstraTerm host. Set a remote engine or reach one through SSH.</EditorNote>
      )}
      <OptionSection title="Container">
        <ComboOption
          {...p}
          name="container"
          label="Container"
          required
          mono
          placeholder="Name or ID"
          suggestions={suggestions}
          loading={containers.isFetching}
          onOpen={() => {
            setWanted(true)
            if (wanted) void containers.refetch()
          }}
          emptyText={containers.isError ? `Container list unavailable — ${containers.error instanceof Error ? containers.error.message : 'type the name'}` : logs ? 'No containers found' : 'No running containers found'}
        />
        <SelectOption
          {...p}
          name="dockerMode"
          label="Session"
          defaultLabel="Interactive shell (docker exec)"
          options={[
            { value: 'exec', label: 'Interactive shell (docker exec)' },
            { value: 'logs', label: 'Follow logs (docker logs -f)' },
          ]}
        />
        {logs ? (
          <NumberOption {...p} name="logTail" label="Show last lines" min={0} max={100000} placeholder="200" hint="History shown before following." />
        ) : (
          <>
            <ComboOption {...p} name="shell" label="Shell" mono placeholder="/bin/sh" suggestions={CONTAINER_SHELLS} hint="A program, or a command line run with /bin/sh -c." />
            <TextOption {...p} name="user" label="User" placeholder="Container default" mono trim hint="User or UID (docker exec -u)." />
          </>
        )}
      </OptionSection>
      <OptionSection title="Engine">
        <ConnectionSelectOption
          {...p}
          name="viaConnectionId"
          label="Through SSH connection"
          noneLabel="Direct (no SSH)"
          filter={(c) => c.protocol === 'ssh'}
          hint="Use the Docker socket of a remote host over SSH (the SSH user needs access to it)."
        />
        <TextOption
          {...p}
          name="dockerHost"
          label={via ? 'Remote socket' : 'Docker host'}
          placeholder={via ? 'unix:///var/run/docker.sock' : 'Local socket'}
          mono
          trim
          hint={via ? 'Socket path on the SSH host (Podman: unix:///run/user/UID/podman/podman.sock).' : 'unix:///var/run/docker.sock, npipe:////./pipe/docker_engine or tcp://host:2375.'}
        />
      </OptionSection>
    </div>
  )
}

function validateDocker(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  if (!optString(c, 'container').trim()) errors['options.container'] = 'Container name or ID is required'
  const host = optString(c, 'dockerHost').trim()
  const via = optString(c, 'viaConnectionId')
  if (host) {
    if (via && !/^unix:\/\/\/\S+$/i.test(host)) errors['options.dockerHost'] = 'Through SSH, use a unix:///path socket'
    else if (!via && !/^(unix|npipe|tcp|http):\/\/\S+$/i.test(host)) errors['options.dockerHost'] = 'Use unix://, npipe:// or tcp:// addresses'
  }
  return errors
}

defineProtocol({
  protocol: 'docker',
  label: 'Docker',
  icon: Container,
  defaultPort: 0,
  group: 'terminal',
  order: 80,
  description: 'Shell or logs of a container',
  component: DockerEditor,
  tabLabel: 'Docker settings',
  profile: { host: 'hidden', port: false, username: false, auth: 'none', kind: 'terminal', network: false },
  validate: validateDocker,
})
