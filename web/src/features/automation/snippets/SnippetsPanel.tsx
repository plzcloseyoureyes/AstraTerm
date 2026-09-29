/*
 * Snippets sidebar panel (AUTO-3 library UI): search, tag filter, folders. Click sends the snippet to the active
 * terminal; Shift+click to the broadcast group (every terminal when broadcasting is off); the context menu has the
 * other targets. Rows show the command (variables highlighted) and flag dangerous commands (guard rules).
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  ChevronRight,
  Copy,
  CopyPlus,
  CornerDownLeft,
  FolderClosed,
  FolderOpen,
  Pencil,
  Plus,
  Search,
  Send,
  SendToBack,
  TextCursorInput,
  Trash2,
  TriangleAlert,
  Users,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import type { Snippet } from '@/api/types'
import { Button } from '@/components/ui/button'
import { ContextMenuItem, ContextMenuSeparator } from '@/components/ui/context-menu'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenuItem, DropdownMenuSeparator } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { Kbd } from '@/components/ui/kbd'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, copyText, errorMessage } from '@/lib/utils'
import { autoKeys, createSnippet, deleteSnippet, useSnippets } from '../api'
import { LibraryRow, type MenuKind } from '../components/LibraryRow'
import { checkDangerous } from '../guard'
import { startLibraryDrag } from '../plugins/dropTarget'
import { sendSnippetTo } from '../send'
import { openSnippetEditor } from '../store'

interface Group {
  folder: string
  items: Snippet[]
}

function matches(s: Snippet, q: string): boolean {
  return `${s.name}\n${s.description}\n${s.content}\n${s.folder}\n${s.tags.join(' ')}`.toLowerCase().includes(q)
}

/** The command's first line with its {{variables}} picked out, plus how many more lines it has. */
function CommandPreview({ content }: { content: string }) {
  const lines = content.split(/\r?\n/).filter((l) => l.trim())
  const more = lines.length - 1
  return (
    <>
      <code className="min-w-0 truncate">
        {(lines[0] ?? '').split(/(\{\{[^}]*\}\})/).map((part, i) => (i % 2 ? <span key={i} className="text-primary">{part}</span> : part))}
      </code>
      {more > 0 && <span className="shrink-0 font-sans text-2xs">+{more} {more === 1 ? 'line' : 'lines'}</span>}
    </>
  )
}

function SnippetRow({ s, onSend }: { s: Snippet; onSend: (s: Snippet, where: 'active' | 'multiexec') => void }) {
  const qc = useQueryClient()
  const danger = React.useMemo(() => checkDangerous(s.content), [s.content])
  const remove = async () => {
    if (!(await confirm({ title: `Delete “${s.name}”?`, destructive: true, confirmLabel: 'Delete' }))) return
    try {
      await deleteSnippet(s.id)
      await qc.invalidateQueries({ queryKey: autoKeys.snippets })
    } catch (err) {
      toast.error('Could not delete the snippet', { description: errorMessage(err) })
    }
  }
  const duplicate = async () => {
    try {
      await createSnippet({ name: `${s.name} (copy)`.slice(0, 200), folder: s.folder, description: s.description, content: s.content, tags: s.tags, sendMode: s.sendMode })
      await qc.invalidateQueries({ queryKey: autoKeys.snippets })
    } catch (err) {
      toast.error('Could not duplicate the snippet', { description: errorMessage(err) })
    }
  }
  const execute = s.sendMode === 'execute'
  return (
    <LibraryRow
      name={s.name}
      icon={execute ? <CornerDownLeft className="text-primary" aria-label="Runs (presses Enter)" /> : <TextCursorInput className="text-muted-foreground" aria-label="Pastes without running" />}
      badge={
        danger.length > 0 && (
          <TriangleAlert className={cn('size-3 shrink-0', danger.some((d) => d.severity === 'danger') ? 'text-destructive' : 'text-warning')} aria-label="Dangerous command" />
        )
      }
      meta={
        s.shortcut ? (
          <Kbd keys={s.shortcut} />
        ) : (
          s.tags.length > 0 && (
            <span className="max-w-24 truncate">
              {s.tags[0]}
              {s.tags.length > 1 && ` +${s.tags.length - 1}`}
            </span>
          )
        )
      }
      detail={<CommandPreview content={s.content} />}
      hint={[
        s.description,
        s.content,
        danger.length ? `⚠ ${danger.map((d) => d.message).join(' ')}` : '',
        `${execute ? 'Runs' : 'Pastes'} · click: active terminal · Shift+click: broadcast group · drag onto any terminal`,
      ]
        .filter(Boolean)
        .join('\n\n')}
      onActivate={(e) => onSend(s, e.shiftKey ? 'multiexec' : 'active')}
      onDragStart={(e) => startLibraryDrag(e, 'snippet', s.id, s.content)}
      menu={(kind) => <SnippetMenuItems s={s} kind={kind} onEdit={() => openSnippetEditor(s)} onDuplicate={duplicate} onDelete={remove} />}
    />
  )
}

function SnippetMenuItems({
  s,
  kind,
  onEdit,
  onDuplicate,
  onDelete,
}: {
  s: Snippet
  kind: MenuKind
  onEdit: () => void
  onDuplicate: () => void
  onDelete: () => void
}) {
  const Item = kind === 'dropdown' ? DropdownMenuItem : ContextMenuItem
  const Sep = kind === 'dropdown' ? DropdownMenuSeparator : ContextMenuSeparator
  return (
    <>
      <Item onSelect={() => void sendSnippetTo(s, 'active')}>
        <Send /> Send to active terminal
      </Item>
      <Item onSelect={() => void sendSnippetTo(s, 'multiexec')}>
        <SendToBack /> Send to broadcast group
      </Item>
      <Item onSelect={() => void sendSnippetTo(s, 'all')}>
        <Users /> Send to all terminals
      </Item>
      <Item onSelect={() => void sendSnippetTo(s, 'pick')}>
        <Users /> Send to sessions…
      </Item>
      <Sep />
      <Item onSelect={onEdit}>
        <Pencil /> Edit…
      </Item>
      <Item onSelect={onDuplicate}>
        <CopyPlus /> Duplicate
      </Item>
      <Item
        onSelect={() => {
          void copyText(s.content).then((ok) => (ok ? toast.success('Copied') : toast.error('Could not copy')))
        }}
      >
        <Copy /> Copy text
      </Item>
      <Sep />
      <Item variant="destructive" onSelect={onDelete}>
        <Trash2 /> Delete
      </Item>
    </>
  )
}

export default function SnippetsPanel() {
  const query = useSnippets()
  const { data } = query
  const [q, setQ] = React.useState('')
  const [tag, setTag] = React.useState<string | null>(null)
  const [collapsed, setCollapsed] = React.useState<Set<string>>(() => new Set())
  const snippets = React.useMemo(() => data ?? [], [data])
  const tags = React.useMemo(() => Array.from(new Set(snippets.flatMap((s) => s.tags))).sort((a, b) => a.localeCompare(b)), [snippets])
  const needle = q.trim().toLowerCase()
  const groups = React.useMemo<Group[]>(() => {
    const shown = snippets.filter((s) => (!needle || matches(s, needle)) && (!tag || s.tags.includes(tag)))
    const map = new Map<string, Snippet[]>()
    for (const s of shown) {
      const list = map.get(s.folder) ?? []
      list.push(s)
      map.set(s.folder, list)
    }
    return Array.from(map.entries())
      .sort(([a], [b]) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)))
      .map(([folder, items]) => ({ folder, items: items.sort((a, b) => a.name.localeCompare(b.name)) }))
  }, [snippets, needle, tag])
  const onSend = (s: Snippet, where: 'active' | 'multiexec') => void sendSnippetTo(s, where)
  const count = groups.reduce((n, g) => n + g.items.length, 0)

  const body = !count ? (
    <EmptyState size="sm" title="Nothing matches" description="Try another search or tag." />
  ) : (
    groups.map((g) => {
      const open = !collapsed.has(g.folder) || !!needle
      const FolderIcon = open ? FolderOpen : FolderClosed
      return (
        <section key={g.folder || '_root'} className="mb-1">
          {g.folder && (
            <button
              type="button"
              aria-expanded={open}
              onClick={() =>
                setCollapsed((prev) => {
                  const next = new Set(prev)
                  if (next.has(g.folder)) next.delete(g.folder)
                  else next.add(g.folder)
                  return next
                })
              }
              className="flex w-full items-center gap-1 rounded-md px-1.5 py-1 text-xs font-semibold text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60"
            >
              <ChevronRight className={`size-3 transition-transform ${open ? 'rotate-90' : ''}`} aria-hidden />
              <FolderIcon className="size-3.5" aria-hidden />
              <span className="truncate">{g.folder}</span>
              <span className="ml-auto text-2xs font-normal">{g.items.length}</span>
            </button>
          )}
          {open && (
            <ul className={g.folder ? 'ml-2 border-l pl-1' : ''}>
              {g.items.map((s) => (
                <SnippetRow key={s.id} s={s} onSend={onSend} />
              ))}
            </ul>
          )}
        </section>
      )
    })
  )

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex h-9 shrink-0 items-center gap-1 px-1.5">
        <Input
          inputSize="sm"
          variant="filled"
          leading={<Search />}
          trailing={
            q ? (
              <IconButton icon={X} label="Clear search" size="xs" onClick={() => setQ('')} />
            ) : undefined
          }
          placeholder="Search snippets…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          aria-label="Search snippets"
          onKeyDown={(e) => {
            if (e.key === 'Escape') setQ('')
          }}
        />
        <IconButton icon={Plus} label="New snippet" onClick={() => openSnippetEditor()} />
      </div>
      {tags.length > 0 && (
        <div className="flex shrink-0 flex-wrap gap-1 border-b px-2 py-1.5" role="group" aria-label="Filter by tag">
          {tags.map((t) => (
            <button
              key={t}
              type="button"
              aria-pressed={tag === t}
              onClick={() => setTag((cur) => (cur === t ? null : t))}
              className="rounded-full border px-2 py-px text-2xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 aria-pressed:border-primary aria-pressed:bg-primary/15 aria-pressed:text-primary"
            >
              {t}
            </button>
          ))}
        </div>
      )}
      <div className="min-h-0 flex-1 overflow-y-auto px-1 py-1">
        <QueryState
          query={query}
          skeleton={<SkeletonRows rows={6} rowHeight={44} />}
          errorTitle="Could not load snippets"
          isEmpty={(list) => !list.length}
          empty={
            <EmptyState
              size="sm"
              icon={TextCursorInput}
              title="No snippets yet"
              description="Save commands you type often and send them to one or all terminals with a click."
              action={
                <Button size="sm" onClick={() => openSnippetEditor()}>
                  <Plus /> New snippet
                </Button>
              }
            />
          }
        >
          {() => body}
        </QueryState>
      </div>
      <div className="flex h-7 shrink-0 items-center justify-between gap-2 border-t px-2 text-2xs text-muted-foreground">
        <span className="tabular-nums">{count ? `${count} snippet${count === 1 ? '' : 's'}` : ''}</span>
        <Tooltip content="Click sends to the active terminal; Shift+click to the broadcast group (every terminal when none is set up). Drag a snippet onto any terminal to run it there.">
          <span className="flex cursor-default items-center gap-1">
            <Kbd>⇧</Kbd> click to broadcast
          </span>
        </Tooltip>
      </div>
    </div>
  )
}
