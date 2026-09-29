/*
 * Session editor (SM-2 / SM-4): protocol icon row, basic settings, then tabs — protocol settings (registered protocol
 * editor), Terminal, Network, Bookmark, Automation. react-hook-form holds the draft, zod validates it (protocol-aware),
 * create / edit / read-only (shared by another user) modes, "Save", "Save & connect", dirty-close confirmation.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { useForm, useWatch, type FieldErrors, type Resolver } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { toast } from 'sonner'
import { CopyPlus, Eye, Plug, Save } from 'lucide-react'
import { protocolEditors } from '@/app/registry'
import { defaultPort, protocolIcon, protocolLabel } from '@/app/protocols'
import { duplicateConnection, useConnection, useConnections, useCreateConnection, useUpdateConnection } from '@/api/connections'
import { useFolders } from '@/api/folders'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection } from '@/api/types'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Kbd } from '@/components/ui/kbd'
import { LoadingPane } from '@/components/ui/spinner'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { errorMessage } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { connectSafely } from '../connect'
import { closeSessionEditor, openSessionEditor, setEditorDirty, type EditorRequest } from '../dialogs/store'
import { EditorEnvContext, type EditorEnv } from '../editors/context'
import { getProtocolProfile, getProtocolSpec, type ProtocolProfile } from '../editors/define'
import { canModify, collectTags, connNodeId, nextSortOrder } from '../model'
import { revealNode } from '../panel/controller'
import { isValidHost, parseQuickConnect } from '../quickparse'
import { sessionsSettings } from '../settings'
import { BasicSection } from './BasicSection'
import { ProtocolPicker } from './ProtocolPicker'
import {
  adaptOptionsForProtocol,
  buildSchema,
  createPayload,
  diffFromConnection,
  formDefaults,
  patchPayload,
  tabOfPath,
  toConnection,
  type SessionFormValues,
} from './schema'
import { AutomationTab, BookmarkTab, NetworkTab, TerminalTab } from './tabs'

type TabId = 'protocol' | 'terminal' | 'network' | 'bookmark' | 'automation'

function tabsFor(profile: ProtocolProfile): TabId[] {
  const tabs: TabId[] = ['protocol']
  if (profile.kind === 'terminal') tabs.push('terminal')
  if (profile.network) tabs.push('network')
  tabs.push('bookmark')
  if (profile.kind !== 'files') tabs.push('automation')
  return tabs
}

/** RHF nested errors → { "options.device": "message", … }. */
function flattenErrors(obj: unknown, prefix = '', out: Record<string, string> = {}): Record<string, string> {
  if (!obj || typeof obj !== 'object') return out
  for (const [k, v] of Object.entries(obj as Record<string, unknown>)) {
    if (k === 'ref' || !v || typeof v !== 'object') continue
    const path = prefix ? `${prefix}.${k}` : k
    const msg = (v as { message?: unknown }).message
    if (typeof msg === 'string' && 'type' in (v as object)) {
      out[path] = msg
      // Item errors of arrays (tags.3) are shown on the array field.
      const parent = path.replace(/\.\d+$/, '')
      if (parent !== path && !out[parent]) out[parent] = msg
    } else flattenErrors(v, path, out)
  }
  return out
}

function stripBrackets(host: string): string {
  return host.startsWith('[') && host.endsWith(']') ? host.slice(1, -1) : host
}

export default function SessionEditorDialog({ request, hidden }: { request: EditorRequest; hidden: boolean }) {
  const isEdit = request.mode === 'edit' && !!request.connectionId
  const q = useConnection(isEdit ? request.connectionId : undefined)
  if (isEdit && !q.data) {
    return (
      <Dialog open={!hidden} onOpenChange={(o) => !o && closeSessionEditor()}>
        <DialogContent size="sm">
          <DialogHeader>
            <DialogTitle>Edit session</DialogTitle>
            <DialogDescription>{q.isError ? errorMessage(q.error, 'The session could not be loaded.') : 'Loading the session…'}</DialogDescription>
          </DialogHeader>
          {q.isError ? (
            <DialogFooter>
              <Button onClick={() => closeSessionEditor()}>Close</Button>
            </DialogFooter>
          ) : (
            <LoadingPane />
          )}
        </DialogContent>
      </Dialog>
    )
  }
  return <EditorForm request={request} original={isEdit ? q.data : undefined} hidden={hidden} />
}

function EditorForm({ request, original, hidden }: { request: EditorRequest; original?: Connection; hidden: boolean }) {
  const user = useCurrentUser()
  const isAdmin = user?.role === 'admin'
  const readOnly = !!original && !canModify(original, user)
  const mode: 'create' | 'edit' = original ? 'edit' : 'create'
  const connections = useConnections().data ?? []
  const folders = useFolders().data ?? []
  const createConn = useCreateConnection()
  const updateConn = useUpdateConnection()
  protocolEditors.useList() // re-render when editors (re)register

  const [defaults] = useState(() =>
    formDefaults({ original, initial: request.initial, folderId: request.folderId, protocol: request.protocol ?? sessionsSettings.get().lastProtocol }),
  )
  // The schema validates the same shape; its inferred option type is looser than ConnectionOptions.
  const resolver = useMemo(() => zodResolver(buildSchema(original)) as unknown as Resolver<SessionFormValues>, [original])
  const formRef = useRef<HTMLFormElement>(null)
  const form = useForm<SessionFormValues>({ defaultValues: defaults, resolver, mode: 'onSubmit', reValidateMode: 'onChange' })
  const { control, formState, setValue, getValues, handleSubmit, trigger } = form
  const watched = useWatch({ control }) as Partial<SessionFormValues>
  const values: SessionFormValues = { ...defaults, ...watched } as SessionFormValues
  const profile = getProtocolProfile(values.protocol)
  const spec = getProtocolSpec(values.protocol)
  const editorDef = protocolEditors.get(values.protocol)
  const conn = toConnection(values, original)
  const errors = flattenErrors(formState.errors)
  const tabs = tabsFor(profile)

  const initialTab = (request.tab && (['protocol', 'terminal', 'network', 'bookmark', 'automation'] as string[]).includes(request.tab) ? request.tab : 'protocol') as TabId
  const [tab, setTab] = useState<TabId>(initialTab)
  const activeTab: TabId = tabs.includes(tab) ? tab : 'protocol'
  const [submitError, setSubmitError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [confirmClose, setConfirmClose] = useState(false)

  useEffect(() => setEditorDirty(formState.isDirty && !readOnly), [formState.isDirty, readOnly])

  const dirtyOpts = { shouldDirty: true, shouldValidate: formState.isSubmitted } as const

  /** Apply a changed draft (from a protocol editor, tab or credentials control). */
  const applyConn = (next: Connection) => {
    if (readOnly) return
    const diff = diffFromConnection(next, getValues())
    for (const [k, v] of Object.entries(diff)) setValue(k as keyof SessionFormValues, v as never, dirtyOpts)
  }

  const switchProtocol = (next: string) => {
    const cur = getValues()
    if (readOnly || cur.protocol === next) return
    const from = getProtocolProfile(cur.protocol)
    const to = getProtocolProfile(next)
    setValue('protocol', next, dirtyOpts)
    if (cur.port !== null && (cur.port === defaultPort(cur.protocol) || !to.port)) setValue('port', null, dirtyOpts)
    setValue('options', adaptOptionsForProtocol(cur.options, next), dirtyOpts)
    if (to.auth !== 'ssh') {
      if (cur.authMethod === 'key' || cur.authMethod === 'agent' || cur.authMethod === 'keyboard-interactive') setValue('authMethod', 'auto', dirtyOpts)
      if (cur.keyId) setValue('keyId', null, dirtyOpts)
    }
    if (!to.identity && cur.identityId) setValue('identityId', null, dirtyOpts)
    // Carry a typed (unsaved) password over when the protocol stores it under another name (vncPassword).
    const fromSecret = from.passwordSecret ?? 'password'
    const toSecret = to.passwordSecret ?? 'password'
    if (fromSecret !== toSecret && cur.secrets[fromSecret]) {
      const secrets = { ...cur.secrets, [toSecret]: cur.secrets[fromSecret] }
      delete secrets[fromSecret]
      setValue('secrets', secrets, dirtyOpts)
    }
    if (formState.isSubmitted) void trigger()
  }

  /** "root@10.0.0.5:2222" or "ssh://…" typed into the host field is split into user / host / port (and protocol). */
  const onHostBlur = (text: string) => {
    const t = text.trim()
    if (!t) return
    if (isValidHost(stripBrackets(t))) {
      if (t !== text) setValue('host', t, dirtyOpts)
      return
    }
    if (!/[@:/]/.test(t)) return
    try {
      const res = parseQuickConnect(t)
      if (res.kind !== 'connect' || !res.draft.host) return
      const d = res.draft
      if (t.includes('://') && d.protocol !== getValues('protocol')) switchProtocol(d.protocol)
      const protocol = getValues('protocol')
      setValue('host', d.host ?? '', dirtyOpts)
      if (d.port) setValue('port', d.port === defaultPort(protocol) ? null : d.port, dirtyOpts)
      if (d.username && !getValues('username').trim()) setValue('username', d.username, dirtyOpts)
      if (d.password) {
        const secret = getProtocolProfile(protocol).passwordSecret ?? 'password'
        setValue('secrets', { ...getValues('secrets'), [secret]: d.password }, dirtyOpts)
      }
    } catch {
      /* leave the text as typed — validation explains what is wrong */
    }
  }

  const save = async (v: SessionFormValues, connectAfter: boolean) => {
    if (readOnly || saving) return
    setSubmitError(null)
    setSaving(true)
    try {
      let saved: Connection
      let created = false
      if (original) {
        const patch = patchPayload(v, original)
        saved = Object.keys(patch).length ? await updateConn.mutateAsync({ id: original.id, patch }) : original
      } else {
        const siblings = connections.filter((c) => (c.folderId || null) === (v.folderId || null))
        saved = await createConn.mutateAsync(createPayload(v, nextSortOrder(siblings)))
        created = true
        sessionsSettings.set({ lastProtocol: v.protocol })
      }
      queryClient.setQueryData(queryKeys.connection(saved.id), saved)
      closeSessionEditor()
      toast.success(created ? `Session "${saved.name}" created` : `Session "${saved.name}" saved`)
      if (created || (original && (original.folderId || null) !== (saved.folderId || null))) revealNode(connNodeId(saved.id))
      if (connectAfter) void connectSafely(saved)
    } catch (err) {
      setSubmitError(errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  const onInvalid = (errs: FieldErrors<SessionFormValues>) => {
    const first = Object.keys(flattenErrors(errs))[0]
    if (!first) return
    const t = tabOfPath(first)
    if (t !== 'basic' && tabs.includes(t)) setTab(t)
    setSubmitError('Please fix the highlighted fields.')
  }

  const onSave = handleSubmit((v) => save(v, false), onInvalid)
  const onSaveConnect = handleSubmit((v) => save(v, true), onInvalid)

  const requestClose = () => {
    if (saving) return
    if (formState.isDirty && !readOnly) setConfirmClose(true)
    else closeSessionEditor()
  }

  const duplicateToEdit = async () => {
    if (!original) return
    try {
      const copy = await duplicateConnection(original.id)
      queryClient.setQueryData<Connection[]>(queryKeys.connections, (old) => (old ? [...old.filter((c) => c.id !== copy.id), copy] : old))
      void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
      closeSessionEditor()
      openSessionEditor({ mode: 'edit', connectionId: copy.id })
      revealNode(connNodeId(copy.id))
    } catch (err) {
      toast.error('Could not duplicate the session', { description: errorMessage(err) })
    }
  }

  const env: EditorEnv = { mode, readOnly, errors, connections, selfId: original?.id }
  const ProtocolComponent = editorDef?.component
  const TitleIcon = protocolIcon(values.protocol)
  const title = readOnly ? `View “${original?.name}”` : original ? `Edit “${original.name}”` : 'New session'
  const tabLabels: Record<TabId, string> = {
    protocol: spec?.tabLabel ?? `${protocolLabel(values.protocol)} settings`,
    terminal: 'Terminal',
    network: 'Network',
    bookmark: 'Bookmark',
    automation: 'Automation',
  }
  const canShare = isAdmin || (!!original?.shared && original.ownerId === user?.id)

  const onFormKeyDown = (e: KeyboardEvent<HTMLFormElement>) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !readOnly) {
      e.preventDefault()
      void onSaveConnect()
    }
  }

  return (
    <>
      <Dialog open={!hidden} onOpenChange={(o) => !o && requestClose()}>
        <DialogContent
          size="2xl"
          className="h-[min(92dvh,880px)] gap-0 overflow-hidden p-0"
          onOpenAutoFocus={(e) => {
            e.preventDefault()
            const root = formRef.current
            const el = root?.querySelector<HTMLInputElement>('[data-autofocus-host]') ?? root?.querySelector<HTMLInputElement>('input[name="name"]')
            el?.focus()
          }}
          onPointerDownOutside={(e) => {
            // Never lose edits to a stray click outside.
            if (formState.isDirty) e.preventDefault()
          }}
        >
          <form ref={formRef} className="flex min-h-0 flex-1 flex-col" onSubmit={onSave} onKeyDown={onFormKeyDown} noValidate>
            <DialogHeader className="border-b px-5 pt-4 pb-3 pr-10">
              <DialogTitle className="min-w-0">
                <TitleIcon className="size-4 shrink-0 text-muted-foreground" />
                <span className="truncate">{title}</span>
                {readOnly && (
                  <Badge variant="info">
                    <Eye /> Shared · read-only
                  </Badge>
                )}
              </DialogTitle>
              <DialogDescription className="sr-only">Connection settings for a saved session</DialogDescription>
              <div className="pt-2">
                <ProtocolPicker value={values.protocol} onChange={switchProtocol} disabled={readOnly} />
              </div>
            </DialogHeader>

            <div className="@container min-h-0 flex-1 overflow-y-auto px-5 py-4">
              <EditorEnvContext.Provider value={env}>
                <fieldset disabled={readOnly} className="m-0 min-w-0 border-0 p-0">
                  <BasicSection form={form} profile={profile} value={conn} onChange={applyConn} errors={errors} onHostBlur={onHostBlur} />
                </fieldset>
                <Tabs value={activeTab} onValueChange={(t) => setTab(t as TabId)} className="mt-4 gap-4">
                  <TabsList className="scrollbar-none overflow-x-auto">
                    {tabs.map((t) => (
                      <TabsTrigger key={t} value={t}>
                        {tabLabels[t]}
                        {Object.keys(errors).some((p) => tabOfPath(p) === t) && <span className="size-1.5 rounded-full bg-destructive" aria-label="has errors" />}
                      </TabsTrigger>
                    ))}
                  </TabsList>
                  <fieldset disabled={readOnly} className="m-0 min-w-0 border-0 p-0">
                    <TabsContent value="protocol">
                      {ProtocolComponent ? (
                        <ProtocolComponent value={conn} onChange={applyConn} mode={mode} />
                      ) : (
                        <p className="text-sm text-muted-foreground">No extra settings for this protocol.</p>
                      )}
                    </TabsContent>
                    {tabs.includes('terminal') && (
                      <TabsContent value="terminal">
                        <TerminalTab value={conn} onChange={applyConn} />
                      </TabsContent>
                    )}
                    {tabs.includes('network') && (
                      <TabsContent value="network">
                        <NetworkTab value={conn} onChange={applyConn} profile={profile} />
                      </TabsContent>
                    )}
                    <TabsContent value="bookmark">
                      <BookmarkTab value={conn} onChange={applyConn} folders={folders} allTags={collectTags(connections)} canShare={canShare} />
                    </TabsContent>
                    {tabs.includes('automation') && (
                      <TabsContent value="automation">
                        <AutomationTab value={conn} onChange={applyConn} profile={profile} />
                      </TabsContent>
                    )}
                  </fieldset>
                </Tabs>
              </EditorEnvContext.Provider>
            </div>

            <DialogFooter className="items-center border-t px-5 py-3 sm:justify-between">
              <div className="min-w-0 flex-1 text-sm">
                {submitError ? (
                  <p role="alert" className="truncate text-destructive" title={submitError}>
                    {submitError}
                  </p>
                ) : !readOnly ? (
                  <p className="hidden items-center gap-1 text-muted-foreground sm:flex">
                    <Kbd keys="$mod+Enter" /> save &amp; connect
                  </p>
                ) : null}
              </div>
              <div className="flex flex-col-reverse gap-2 sm:flex-row">
                {readOnly ? (
                  <>
                    <Button variant="secondary" onClick={() => closeSessionEditor()}>
                      Close
                    </Button>
                    <Button variant="secondary" onClick={() => void duplicateToEdit()}>
                      <CopyPlus /> Duplicate to edit
                    </Button>
                    <Button
                      onClick={() => {
                        closeSessionEditor()
                        if (original) void connectSafely(original)
                      }}
                    >
                      <Plug /> Connect
                    </Button>
                  </>
                ) : (
                  <>
                    <Button variant="secondary" onClick={requestClose} disabled={saving}>
                      Cancel
                    </Button>
                    <Button type="submit" variant="secondary" loading={saving}>
                      <Save /> Save
                    </Button>
                    <Button onClick={() => void onSaveConnect()} disabled={saving}>
                      <Plug /> Save &amp; connect
                    </Button>
                  </>
                )}
              </div>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={confirmClose && !hidden} onOpenChange={setConfirmClose}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Discard changes?</AlertDialogTitle>
            <AlertDialogDescription>Your unsaved changes to this session will be lost.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep editing</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => closeSessionEditor()}>
              Discard
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
