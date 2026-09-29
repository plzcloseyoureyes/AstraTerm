import { useEffect, useState } from 'react'
import { ChevronRight, CircleCheck, Globe, Link2, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { SimpleSelect } from '@/components/ui/select'
import { SwitchField } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { cn, errorMessage } from '@/lib/utils'
import { createOidc, testOidc, updateOidc } from '@/features/security/api'
import { ReauthCancelled } from '@/features/security/store'
import { CopyButton, Notice } from '@/features/security/components'
import type { OidcProvider, OidcProviderInput, OidcTestResult } from '@/features/security/types'

const EMPTY: OidcProviderInput = {
  name: '',
  enabled: true,
  issuer: '',
  clientId: '',
  scopes: ['openid', 'profile', 'email'],
  usernameClaim: 'preferred_username',
  emailClaim: 'email',
  displayNameClaim: 'name',
  groupsClaim: 'groups',
  adminGroups: [],
  allowedGroups: [],
  syncRole: false,
  autoProvision: true,
  linkByUsername: false,
  requireVerifiedEmail: false,
  prompt: '',
}

export function ProviderDialog({ value, redirectUri, onClose, onSaved }: { value: OidcProvider | 'new' | null; redirectUri: string; onClose: () => void; onSaved: () => void }) {
  const isNew = value === 'new'
  const [draft, setDraft] = useState<OidcProviderInput>(EMPTY)
  const [secret, setSecret] = useState('')
  const [clearSecret, setClearSecret] = useState(false)
  const [advanced, setAdvanced] = useState(false)
  const [test, setTest] = useState<OidcTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!value) return
    if (value === 'new') setDraft(EMPTY)
    else {
      const { hasClientSecret: _h, createdAt: _c, updatedAt: _u, ...rest } = value
      void _h
      void _c
      void _u
      setDraft(rest)
    }
    setSecret('')
    setClearSecret(false)
    setAdvanced(false)
    setTest(null)
    setError(null)
  }, [value])

  const set = <K extends keyof OidcProviderInput>(k: K, v: OidcProviderInput[K]) => setDraft((d) => ({ ...d, [k]: v }))
  const hadSecret = value !== 'new' && !!value?.hasClientSecret
  const valid = !!draft.name?.trim() && /^https?:\/\/\S+$/.test(draft.issuer?.trim() ?? '') && !!draft.clientId?.trim()

  const check = async () => {
    if (!draft.issuer?.trim()) return
    setTesting(true)
    try {
      setTest(await testOidc({ issuer: draft.issuer.trim() }))
    } catch (err) {
      setTest({ ok: false, error: errorMessage(err), latencyMs: 0, redirectUri })
    } finally {
      setTesting(false)
    }
  }

  const save = async () => {
    if (!valid || busy) return
    setBusy(true)
    setError(null)
    const body: OidcProviderInput = { ...draft, name: draft.name?.trim(), issuer: draft.issuer?.trim(), clientId: draft.clientId?.trim() }
    if (secret) body.clientSecret = secret
    else if (clearSecret) body.clientSecret = ''
    try {
      if (isNew) await createOidc(body)
      else if (value) await updateOidc(value.id, body)
      toast.success(isNew ? `${body.name} added` : `${body.name} saved`, { description: isNew && body.enabled ? 'It now shows on the sign-in screen.' : undefined })
      onSaved()
      onClose()
    } catch (err) {
      if (err instanceof ReauthCancelled) return
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={!!value} onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="lg">
        <form
          className="flex min-h-0 flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <Link2 className="size-4" /> {isNew ? 'Add identity provider' : `Edit ${(value as OidcProvider | null)?.name ?? ''}`}
            </DialogTitle>
            <DialogDescription>Register NexTerm as a confidential web application (authorization code flow) at your provider.</DialogDescription>
          </DialogHeader>
          <DialogBody className="grid max-h-[65vh] gap-4 overflow-y-auto">
            <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/30 px-3 py-2 text-sm">
              <span className="text-muted-foreground">Redirect URI:</span>
              <code className="min-w-0 truncate font-mono text-xs">{redirectUri}</code>
              <CopyButton text={redirectUri} size="xs" variant="ghost" />
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Button label" hint="Shown as “Continue with …”." required>
                <Input value={draft.name ?? ''} onChange={(e) => set('name', e.target.value)} placeholder="Okta" autoFocus />
              </Field>
              <Field label="Client ID" required>
                <Input value={draft.clientId ?? ''} onChange={(e) => set('clientId', e.target.value)} autoComplete="off" spellCheck={false} />
              </Field>
            </div>
            <Field label="Issuer URL" hint="Its /.well-known/openid-configuration must be reachable from the NexTerm server." required>
              <div className="flex gap-2">
                <Input
                  value={draft.issuer ?? ''}
                  onChange={(e) => {
                    set('issuer', e.target.value)
                    setTest(null)
                  }}
                  placeholder="https://login.example.com/realms/main"
                  spellCheck={false}
                  className="flex-1"
                />
                <Button variant="secondary" onClick={() => void check()} loading={testing} disabled={!draft.issuer?.trim()}>
                  <Globe /> Check
                </Button>
              </div>
            </Field>
            {test && (
              <Notice tone={test.ok ? 'info' : 'destructive'} icon={test.ok ? CircleCheck : TriangleAlert}>
                {test.ok ? (
                  <span>
                    Found <strong>{test.issuer}</strong> in {test.latencyMs} ms
                    {test.pkceMethods && !test.pkceMethods.includes('S256') ? ' — the provider does not advertise PKCE S256.' : '.'}
                  </span>
                ) : (
                  <span>{test.error || 'The provider could not be reached.'}</span>
                )}
              </Notice>
            )}
            <Field
              label="Client secret"
              hint={hadSecret ? 'A secret is saved. Type a new one to replace it.' : 'Leave empty for a public client (PKCE only).'}
            >
              <PasswordInput
                value={secret}
                onChange={(e) => {
                  setSecret(e.target.value)
                  setClearSecret(false)
                }}
                placeholder={hadSecret && !clearSecret ? '•••••••• (saved)' : ''}
                autoComplete="new-password"
              />
            </Field>
            {hadSecret && !secret && (
              <label className="-mt-2 flex items-center gap-2 text-sm text-muted-foreground">
                <input type="checkbox" className="size-3.5 accent-[var(--primary)]" checked={clearSecret} onChange={(e) => setClearSecret(e.target.checked)} />
                Remove the saved secret
              </label>
            )}
            <div className="grid gap-3 rounded-lg border p-3">
              <SwitchField label="Enabled" description="Show it on the sign-in screen." checked={!!draft.enabled} onCheckedChange={(v) => set('enabled', v)} />
              <SwitchField
                label="Create accounts on first sign-in"
                description="Otherwise only people with a linked or pre-created account can sign in."
                checked={!!draft.autoProvision}
                onCheckedChange={(v) => set('autoProvision', v)}
              />
              <Field label="Administrator groups" hint="Members of these groups become administrators.">
                <TagInput value={draft.adminGroups ?? []} onChange={(v) => set('adminGroups', v)} placeholder="nexterm-admins" aria-label="Administrator groups" />
              </Field>
              <SwitchField
                label="Update the role on every sign-in"
                description="Promote / demote from the groups each time (the last administrator is never demoted)."
                checked={!!draft.syncRole}
                onCheckedChange={(v) => set('syncRole', v)}
              />
            </div>
            <button
              type="button"
              className="flex items-center gap-1.5 justify-self-start text-sm font-medium text-muted-foreground hover:text-foreground"
              onClick={() => setAdvanced((a) => !a)}
              aria-expanded={advanced}
            >
              <ChevronRight className={cn('size-4 transition-transform duration-150', advanced && 'rotate-90')} /> Advanced
            </button>
            {advanced && (
              <div className="grid gap-4 animate-in fade-in-0 duration-150">
                <Field label="Scopes">
                  <TagInput value={draft.scopes ?? []} onChange={(v) => set('scopes', v)} aria-label="Scopes" />
                </Field>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label="User name claim">
                    <Input value={draft.usernameClaim ?? ''} onChange={(e) => set('usernameClaim', e.target.value)} spellCheck={false} />
                  </Field>
                  <Field label="Display name claim">
                    <Input value={draft.displayNameClaim ?? ''} onChange={(e) => set('displayNameClaim', e.target.value)} spellCheck={false} />
                  </Field>
                  <Field label="E-mail claim">
                    <Input value={draft.emailClaim ?? ''} onChange={(e) => set('emailClaim', e.target.value)} spellCheck={false} />
                  </Field>
                  <Field label="Groups claim">
                    <Input value={draft.groupsClaim ?? ''} onChange={(e) => set('groupsClaim', e.target.value)} spellCheck={false} />
                  </Field>
                </div>
                <Field label="Allowed groups" hint="If set, only members of these groups may sign in.">
                  <TagInput value={draft.allowedGroups ?? []} onChange={(v) => set('allowedGroups', v)} placeholder="Everyone" aria-label="Allowed groups" />
                </Field>
                <Field label="Prompt">
                  <SimpleSelect
                    value={draft.prompt || 'default'}
                    onValueChange={(v) => set('prompt', (v === 'default' ? '' : v) as OidcProvider['prompt'])}
                    options={[
                      { value: 'default', label: 'Provider default' },
                      { value: 'login', label: 'Always ask to sign in' },
                      { value: 'select_account', label: 'Choose an account' },
                      { value: 'consent', label: 'Ask for consent' },
                    ]}
                    className="w-56"
                    aria-label="Prompt"
                  />
                </Field>
                <SwitchField
                  label="Require a verified e-mail"
                  description="Refuse identities whose email_verified claim is not true."
                  checked={!!draft.requireVerifiedEmail}
                  onCheckedChange={(v) => set('requireVerifiedEmail', v)}
                />
                <SwitchField
                  label="Link existing accounts by user name"
                  description="A first sign-in whose user name matches a local account signs in to that account."
                  checked={!!draft.linkByUsername}
                  onCheckedChange={(v) => set('linkByUsername', v)}
                />
                {draft.linkByUsername && (
                  <Notice tone="warning" icon={TriangleAlert}>
                    Only enable this when users cannot choose their own user name at the provider — otherwise someone could take over an account
                    (including an administrator) by picking its name.
                  </Notice>
                )}
              </div>
            )}
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={busy} disabled={!valid}>
              {isNew ? 'Add provider' : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
