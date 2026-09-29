import { Fragment, useDeferredValue, useMemo, useState } from 'react'
import { Search, SearchX } from 'lucide-react'
import { SETTINGS_GROUPS, settingsSections, type SettingsGroupId, type SettingsSectionDef, type TabProps } from '@/app/registry'
import { ErrorBoundary } from '@/components/error-boundary'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { LazyBoundary, Spinner } from '@/components/ui/spinner'
import { DELAY_PRESETS } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { updateTabParams } from '@/stores/workspace'
import type { SettingsTabParams } from './index'

/** Groups of sections registered without `group` (features that predate the grouping); anything else unknown goes to
 *  Integrations. */
const DEFAULT_GROUP: Record<string, SettingsGroupId> = {
  editor: 'terminal',
  automation: 'terminal',
  termTransfer: 'files',
  recordings: 'security',
  ai: 'integrations',
  webproxy: 'integrations',
}

const groupOf = (s: SettingsSectionDef): SettingsGroupId => s.group ?? DEFAULT_GROUP[s.id] ?? 'integrations'

interface NavGroup {
  id: SettingsGroupId
  title: string
  sections: SettingsSectionDef[]
}

/** Sections by group (display order), filtered by the search: a section matches by its title and keywords, and every
 *  section of a group matches the group's title. */
function groupSections(sections: SettingsSectionDef[], query: string): NavGroup[] {
  const needle = query.trim().toLowerCase()
  const hit = (s: string) => s.toLowerCase().includes(needle)
  return SETTINGS_GROUPS.map((g) => ({
    id: g.id,
    title: g.title,
    sections: sections
      .filter((s) => groupOf(s) === g.id)
      .filter((s) => !needle || hit(g.title) || hit(s.title) || (s.keywords ?? []).some(hit))
      .sort((a, b) => a.order - b.order),
  })).filter((g) => g.sections.length > 0)
}

/** Settings tab: grouped, searchable section navigation (from the registry) + the active section. */
export default function SettingsView({ tabId, params }: TabProps<SettingsTabParams>) {
  const all = settingsSections.useList()
  const isAdmin = useIsAdmin()
  const [query, setQuery] = useState('')
  const sections = useMemo(() => all.filter((s) => !s.adminOnly || isAdmin), [all, isAdmin])
  const groups = useMemo(() => groupSections(sections, query), [sections, query])
  const visible = useMemo(() => groups.flatMap((g) => g.sections), [groups])

  const ordered = useMemo(() => groupSections(sections, ''), [sections])
  const requested = sections.find((s) => s.id === params?.section) ?? ordered[0]?.sections[0]
  const active = query && requested && !visible.includes(requested) ? visible[0] : requested

  // The page keeps showing the previous section until the next one (a lazy chunk) is ready, instead of blanking in
  // between; the navigation marks the new one at once (and, for a slow load, shows a spinner after 1 s).
  const shownId = useDeferredValue(active?.id)
  const shown = sections.find((s) => s.id === shownId) ?? active
  const switching = !!active && shown?.id !== active.id

  const select = (id: string) => updateTabParams<SettingsTabParams>(tabId, { section: id })

  return (
    <div className="@container h-full">
      <div className="flex h-full min-h-0 flex-col @3xl:flex-row">
        <nav aria-label="Settings sections" className="flex shrink-0 flex-col gap-2 border-b bg-sidebar/60 p-2.5 @3xl:w-60 @3xl:border-r @3xl:border-b-0">
          <Input
            inputSize="sm"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search settings"
            aria-label="Search settings"
            leading={<Search />}
            onKeyDown={(e) => {
              if (e.key === 'Escape') setQuery('')
              if (e.key === 'Enter' && visible[0]) select(visible[0].id)
            }}
          />
          {/* Narrow: one scrolling row, groups separated by a thin rule. Wide: a column with group headings. */}
          <div className="scrollbar-none flex items-center gap-0.5 overflow-x-auto @3xl:-mx-1 @3xl:min-h-0 @3xl:flex-col @3xl:items-stretch @3xl:gap-3 @3xl:overflow-y-auto @3xl:px-1">
            {groups.map((g, gi) => (
              <Fragment key={g.id}>
                {gi > 0 && <span aria-hidden className="mx-1 h-4 w-px shrink-0 bg-border @3xl:hidden" />}
                <div role="group" aria-labelledby={`settings-group-${tabId}-${g.id}`} className="flex shrink-0 gap-0.5 @3xl:flex-col">
                  <h2
                    id={`settings-group-${tabId}-${g.id}`}
                    className="sr-only px-2 pb-0.5 text-2xs font-semibold tracking-wide text-muted-foreground uppercase @3xl:not-sr-only"
                  >
                    {g.title}
                  </h2>
                  {g.sections.map((s) => (
                    <SectionLink key={s.id} section={s} selected={active?.id === s.id} loading={switching && active?.id === s.id} onSelect={select} />
                  ))}
                </div>
              </Fragment>
            ))}
            {groups.length === 0 && <p className="px-2 py-1 text-sm text-muted-foreground">No matching sections</p>}
          </div>
        </nav>
        <div className="@container relative min-h-0 min-w-0 flex-1 overflow-y-auto">
          {shown ? (
            <ErrorBoundary label={shown.title} resetKey={shown.id}>
              {/* The first section of a fresh tab has nothing to keep on screen: its spinner follows the navigation timing. */}
              <LazyBoundary timing={DELAY_PRESETS.NAVIGATION}>
                <shown.component key={shown.id} />
              </LazyBoundary>
            </ErrorBoundary>
          ) : (
            <EmptyState icon={SearchX} title="No settings found" description="Try a different search term." />
          )}
        </div>
      </div>
    </div>
  )
}

function SectionLink({
  section: s,
  selected,
  loading,
  onSelect,
}: {
  section: SettingsSectionDef
  selected: boolean
  loading: boolean
  onSelect: (id: string) => void
}) {
  const Icon = s.icon
  return (
    <button
      type="button"
      onClick={() => onSelect(s.id)}
      aria-current={selected ? 'page' : undefined}
      className={cn(
        'flex h-7 w-full shrink-0 items-center gap-2 rounded-md px-2 text-left text-base outline-none transition-colors',
        'hover:bg-sidebar-accent focus-visible:ring-2 focus-visible:ring-ring/60',
        selected ? 'bg-sidebar-accent font-medium text-foreground' : 'text-foreground/80',
      )}
    >
      <Icon className={cn('size-3.5 shrink-0', selected ? 'text-primary' : 'text-muted-foreground')} />
      <span className="min-w-0 flex-1 truncate">{s.title}</span>
      <Spinner active={loading} {...DELAY_PRESETS.NAVIGATION} className="size-3" label={`Loading ${s.title}`} />
    </button>
  )
}
