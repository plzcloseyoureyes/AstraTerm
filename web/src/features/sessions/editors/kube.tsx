/*
 * Kubernetes editor (PROTO-31): pick context, namespace, pod and container (kubectl on the AstraTerm host), then open a
 * shell (kubectl exec) or follow the logs.
 */
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Boxes } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { listKubeContexts, listKubePods } from '@/api/sessions'
import type { Connection } from '@/api/types'
import { useServerFeatures } from '@/stores/auth'
import { listKubeNamespaces } from '@/features/protocols/api'
import { defineProtocol, type ValidationErrors } from './define'
import { ComboOption, EditorNote, NumberOption, OptionSection, SelectOption, optString, type ComboSuggestion } from './fields'
import { CONTAINER_SHELLS } from './docker'

const errText = (e: unknown, fallback: string) => (e instanceof Error && e.message ? e.message : fallback)

function KubeEditor({ value, onChange }: ProtocolEditorProps) {
  const features = useServerFeatures()
  const p = { value, onChange }
  const context = optString(value, 'context').trim()
  const namespace = optString(value, 'namespace').trim()
  const podName = optString(value, 'pod').trim()
  const logs = optString(value, 'kubeMode') === 'logs'
  const [wantContexts, setWantContexts] = useState(false)
  const [wantNamespaces, setWantNamespaces] = useState(false)
  const [wantPods, setWantPods] = useState(false)

  const contexts = useQuery({ queryKey: ['kube', 'contexts'], queryFn: listKubeContexts, enabled: wantContexts, staleTime: 60_000, retry: false })
  const namespaces = useQuery({
    queryKey: ['kube', 'namespaces', context],
    placeholderData: undefined, // never show another context's namespaces under this key
    queryFn: () => listKubeNamespaces(context || undefined),
    enabled: wantNamespaces,
    staleTime: 30_000,
    retry: false,
  })
  const pods = useQuery({
    queryKey: ['kube', 'pods', context, namespace],
    placeholderData: undefined, // never show another namespace's pods under this key
    queryFn: () => listKubePods(context || undefined, namespace || undefined),
    enabled: wantPods || !!podName,
    staleTime: 10_000,
    retry: false,
  })

  const contextSuggestions: ComboSuggestion[] = (contexts.data ?? []).map((c) => ({
    value: c.name,
    description: [c.current ? 'current' : '', c.cluster ?? '', c.namespace ? `ns ${c.namespace}` : ''].filter(Boolean).join(' · ') || undefined,
  }))
  const namespaceSuggestions: ComboSuggestion[] = (namespaces.data ?? []).map((n) => ({
    value: n.name,
    description: n.status && n.status !== 'Active' ? n.status : undefined,
  }))
  const podSuggestions: ComboSuggestion[] = (pods.data ?? []).map((pod) => ({
    value: pod.name,
    description: [namespace ? '' : pod.namespace, pod.status ?? ''].filter(Boolean).join(' · ') || undefined,
  }))
  const selectedPod = pods.data?.find((x) => x.name === podName)
  const containerSuggestions: ComboSuggestion[] = (selectedPod?.containers ?? []).map((c) => ({ value: c }))

  return (
    <div className="grid gap-5">
      {features && features.kubectl === false && (
        <EditorNote tone="warning">kubectl was not found on the AstraTerm host; Kubernetes sessions need it installed and configured.</EditorNote>
      )}
      <OptionSection title="Target">
        <ComboOption
          {...p}
          name="context"
          label="Context"
          mono
          placeholder="Current context"
          suggestions={contextSuggestions}
          loading={contexts.isFetching}
          onOpen={() => setWantContexts(true)}
          emptyText={contexts.isError ? errText(contexts.error, 'Context list unavailable') : 'No contexts found'}
        />
        <ComboOption
          {...p}
          name="namespace"
          label="Namespace"
          mono
          placeholder="Context default"
          suggestions={namespaceSuggestions}
          loading={namespaces.isFetching}
          onOpen={() => setWantNamespaces(true)}
          emptyText={namespaces.isError ? errText(namespaces.error, 'Namespace list unavailable') : 'No namespaces found'}
        />
        <ComboOption
          {...p}
          name="pod"
          label="Pod"
          required
          mono
          placeholder="Pod name"
          suggestions={podSuggestions}
          loading={pods.isFetching}
          onOpen={() => setWantPods(true)}
          emptyText={pods.isError ? errText(pods.error, 'Pod list unavailable — type the pod name') : 'No pods found'}
        />
        <ComboOption
          {...p}
          name="container"
          label="Container"
          mono
          placeholder="Default container"
          suggestions={containerSuggestions}
          emptyText={podName ? 'Pick a listed pod to see its containers' : 'Choose a pod first'}
        />
      </OptionSection>
      <OptionSection title="Session">
        <SelectOption
          {...p}
          name="kubeMode"
          label="Mode"
          defaultLabel="Interactive shell (kubectl exec)"
          options={[
            { value: 'exec', label: 'Interactive shell (kubectl exec)' },
            { value: 'logs', label: 'Follow logs (kubectl logs -f)' },
          ]}
        />
        {logs ? (
          <NumberOption {...p} name="logTail" label="Show last lines" min={0} max={100000} placeholder="200" />
        ) : (
          <ComboOption {...p} name="shell" label="Shell" mono placeholder="/bin/sh" suggestions={CONTAINER_SHELLS} />
        )}
      </OptionSection>
    </div>
  )
}

function validateKube(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  if (!optString(c, 'pod').trim()) errors['options.pod'] = 'Pod name is required'
  const ns = optString(c, 'namespace').trim()
  if (ns && !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(ns)) errors['options.namespace'] = 'Namespaces use lowercase letters, digits and "-"'
  for (const key of ['context', 'pod', 'container']) {
    if (optString(c, key).trim().startsWith('-')) errors[`options.${key}`] = 'Must not start with "-"'
  }
  return errors
}

defineProtocol({
  protocol: 'kube',
  label: 'Kubernetes',
  icon: Boxes,
  defaultPort: 0,
  group: 'terminal',
  order: 90,
  description: 'Shell or logs of a pod (kubectl)',
  component: KubeEditor,
  tabLabel: 'Kubernetes settings',
  profile: { host: 'hidden', port: false, username: false, auth: 'none', kind: 'terminal', network: false },
  validate: validateKube,
})
