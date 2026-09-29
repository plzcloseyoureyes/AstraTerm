/*
 * Local shell editor (PROTO-14): a shell on the AstraTerm host. Shells are detected by GET /api/local/shells.
 */
import { SquareTerminal } from 'lucide-react'
import type { ProtocolEditorProps } from '@/app/registry'
import { useLocalShells } from '@/api/sessions'
import type { Connection } from '@/api/types'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { defineProtocol, type ValidationErrors } from './define'
import { ComboOption, EditorNote, KeyValueOption, OptionSection, SwitchOption, TagsOption, TextOption, validateEnv, type ComboSuggestion } from './fields'

export function LocalEditor({ value, onChange }: ProtocolEditorProps) {
  const shells = useLocalShells(true)
  const mode = useRunMode()
  const isAdmin = useIsAdmin()
  const p = { value, onChange }
  const suggestions: ComboSuggestion[] = (shells.data ?? []).map((s) => ({ value: s.id, label: s.name, description: s.path }))
  return (
    <div className="grid gap-5">
      {mode === 'server' && !isAdmin && (
        <EditorNote tone="warning">In server mode, local shells on the AstraTerm host are available to administrators only.</EditorNote>
      )}
      <OptionSection title="Shell">
        <ComboOption
          {...p}
          name="shell"
          label="Shell"
          mono
          placeholder="Default shell"
          suggestions={suggestions}
          loading={shells.isLoading}
          emptyText={shells.isError ? 'Shell list unavailable — type a path' : 'No shells detected'}
          hint="A detected shell or the full path of an executable."
        />
        <TextOption {...p} name="cwd" label="Start directory" placeholder="Home directory" mono trim />
        <TagsOption
          {...p}
          name="args"
          label="Arguments"
          placeholder="e.g. -l (Enter adds)"
          hint="One argument per chip, passed as-is (no shell quoting)."
          className="@lg:col-span-2"
        />
        <SwitchOption
          {...p}
          name="loginShell"
          label="Login shell"
          hint="Start as a login shell (reads profile files) when no arguments are given. Unix hosts."
          defaultValue
          className="@lg:col-span-2"
        />
      </OptionSection>
      <OptionSection title="Environment" columns={1}>
        <KeyValueOption {...p} name="env" label="Environment variables" hint="Added to the AstraTerm process environment." />
      </OptionSection>
    </div>
  )
}

function validateLocal(c: Connection): ValidationErrors {
  const errors: ValidationErrors = {}
  const envErr = validateEnv(c)
  if (envErr) errors['options.env'] = envErr
  return errors
}

defineProtocol({
  protocol: 'local',
  label: 'Local shell',
  icon: SquareTerminal,
  defaultPort: 0,
  group: 'terminal',
  order: 60,
  description: 'Shell on the AstraTerm host',
  component: LocalEditor,
  tabLabel: 'Shell settings',
  profile: { host: 'hidden', port: false, username: false, auth: 'none', kind: 'terminal', network: false },
  validate: validateLocal,
})
