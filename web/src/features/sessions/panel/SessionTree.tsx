/*
 * The saved-session tree (SM-1): @headless-tree/react for keyboard navigation, multi-select, inline rename and
 * drag & drop (move + ordered reorder), rendered through @tanstack/react-virtual so thousands of sessions stay fast.
 *
 * Expansion is persisted (settings.sessions.expanded) except while a filter is active, when every matching folder is
 * shown expanded and collapsing is only temporary.
 */
import { useEffect, useImperativeHandle, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent, type MouseEvent, type Ref } from 'react'
import { useTree } from '@headless-tree/react'
import {
  dragAndDropFeature,
  expandAllFeature,
  hotkeysCoreFeature,
  renamingFeature,
  selectionFeature,
  syncDataLoaderFeature,
  type DragTarget,
  type ItemInstance,
} from '@headless-tree/core'
import { useVirtualizer, type Virtualizer } from '@tanstack/react-virtual'
import { ChevronRight, Users } from 'lucide-react'
import { toast } from 'sonner'
import type { Connection, RuntimeSession, User } from '@/api/types'
import { clamp, cn, errorMessage, isEditableTarget } from '@/lib/utils'
import { deleteConnections, deleteFolder, renameConnection, renameFolder } from '../actions'
import { applyMovePlan, planMove } from '../api'
import { connectSafely } from '../connect'
import { openFolderDialog, openSessionEditor } from '../dialogs/store'
import { FolderIcon, safeColor } from '../icons'
import type { SessionMenuContext } from '../menus'
import { canModify, folderNodeId, parseNodeId, type TreeIndex } from '../model'
import { ROOT_ID, type NodeId, type TreeNode } from '../types'
import { useTreeFocus } from './controller'
import { ConnectionLabel } from './rows'

const INDENT = 12

export interface TreeHandle {
  /** Select, expand ancestors and scroll to a node (retries until the data contains it). */
  reveal(nodeId: NodeId): void
  startRename(nodeId: NodeId): boolean
  expandAll(): void
  collapseAll(): void
  /** Focus the tree (the focused row). */
  focus(): void
}

export interface SessionTreeProps {
  index: TreeIndex
  running: Map<string, RuntimeSession[]>
  user: User | null
  /** A filter is active: drag & drop off, expansion not persisted. */
  filtered: boolean
  /** Manual sort order (enables ordered drops). */
  manualOrder: boolean
  expandedFolders: readonly string[]
  onExpandedChange: (folderIds: string[]) => void
  rowHeight: number
  onContextTarget: (ctx: SessionMenuContext) => void
  /** Printable key typed in the tree: continue in the search box. */
  onTypeToSearch: (text: string) => void
  handleRef?: Ref<TreeHandle>
}

function nodeName(n: TreeNode | undefined): string {
  if (!n) return ''
  if (n.kind === 'folder') return n.folder.name
  if (n.kind === 'connection') return n.connection.name
  return ''
}

function ownerOf(n: TreeNode | undefined): { ownerId: string } | undefined {
  if (n?.kind === 'folder') return n.folder
  if (n?.kind === 'connection') return n.connection
  return undefined
}

export function SessionTree({
  index,
  running,
  user,
  filtered,
  manualOrder,
  expandedFolders,
  onExpandedChange,
  rowHeight,
  onContextTarget,
  onTypeToSearch,
  handleRef,
}: SessionTreeProps) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const virtualizerRef = useRef<Virtualizer<HTMLDivElement, Element> | null>(null)

  // --- expansion -------------------------------------------------------------------------------------------------
  const allFolders = useMemo(() => [...index.nodes.keys()].filter((k) => k.startsWith('f:')), [index])
  const [collapsedWhileFiltered, setCollapsedWhileFiltered] = useState<ReadonlySet<string>>(() => new Set())
  useEffect(() => {
    if (!filtered) setCollapsedWhileFiltered(new Set())
  }, [filtered])
  // Stable array identities matter: headless-tree rebuilds whenever `expandedItems` changes by reference.
  const expandedIds = useMemo(
    () => (filtered ? allFolders.filter((id) => !collapsedWhileFiltered.has(id)) : expandedFolders.map(folderNodeId).filter((id) => index.nodes.has(id))),
    [filtered, allFolders, collapsedWhileFiltered, expandedFolders, index],
  )
  const expandedRef = useRef(expandedIds)
  expandedRef.current = expandedIds

  const setExpanded = (next: string[]) => {
    if (filtered) {
      const open = new Set(next)
      setCollapsedWhileFiltered(new Set(allFolders.filter((id) => !open.has(id))))
    } else {
      onExpandedChange([...new Set(next.filter((id) => id.startsWith('f:')).map((id) => id.slice(2)))])
    }
  }

  const modifiable = (item: ItemInstance<TreeNode>) => canModify(ownerOf(item.getItemData()), user)

  // --- drag & drop -----------------------------------------------------------------------------------------------
  const onDrop = async (items: ItemInstance<TreeNode>[], target: DragTarget<TreeNode>) => {
    const targetId = target.item.getId()
    const targetFolder = targetId === ROOT_ID ? null : parseNodeId(targetId).id
    const dragged = [...items].sort((a, b) => a.getItemMeta().index - b.getItemMeta().index).map((i) => i.getId())
    const moveFolderIds = dragged.filter((id) => id.startsWith('f:')).map((id) => id.slice(2))
    const moveConnectionIds = dragged.filter((id) => id.startsWith('c:')).map((id) => id.slice(2))
    let order: { kind: 'folder' | 'connection'; id: string }[] | undefined
    if (manualOrder && 'childIndex' in target) {
      const remaining = (index.children.get(targetId) ?? []).filter((id) => !dragged.includes(id))
      const at = clamp(target.insertionIndex, 0, remaining.length)
      order = [...remaining.slice(0, at), ...dragged, ...remaining.slice(at)].map((id) => ({
        kind: id.startsWith('f:') ? ('folder' as const) : ('connection' as const),
        id: id.slice(2),
      }))
    }
    const plan = planMove({
      folders: [...index.folders.values()],
      connections: [...index.connections.values()],
      targetFolderId: targetFolder,
      moveFolderIds,
      moveConnectionIds,
      order,
    })
    if (!plan.folders.length && !plan.connections.length) return
    if (targetFolder && !expandedRef.current.includes(targetId)) setExpanded([...expandedRef.current, targetId])
    try {
      await applyMovePlan(plan)
    } catch (err) {
      toast.error('Could not move', { description: errorMessage(err) })
    }
  }

  // --- tree ------------------------------------------------------------------------------------------------------
  const tree = useTree<TreeNode>({
    rootItemId: ROOT_ID,
    getItemName: (item) => nodeName(item.getItemData()),
    isItemFolder: (item) => item.getItemData()?.kind === 'folder',
    dataLoader: {
      getItem: (id) => index.nodes.get(id) ?? { kind: 'missing', id },
      getChildren: (id) => index.children.get(id) ?? [],
    },
    state: { expandedItems: expandedIds },
    setExpandedItems: (upd) => setExpanded(typeof upd === 'function' ? upd(expandedRef.current) : upd),
    indent: INDENT,
    canReorder: manualOrder && !filtered,
    canDrag: (items) => !filtered && items.length > 0 && items.every(modifiable),
    canDrop: (_items, target) => !filtered && (target.item.getId() === ROOT_ID || target.item.getItemData()?.kind === 'folder'),
    onDrop,
    // Firefox only starts HTML5 drags that carry data; other features may accept these ids.
    createForeignDragObject: (items) => ({
      format: 'application/x-astraterm-sessions',
      data: JSON.stringify(items.map((i) => i.getId())),
      effectAllowed: 'move',
    }),
    openOnDropDelay: 600,
    canRename: (item) => item.getItemData()?.kind !== 'missing' && modifiable(item),
    onRename: (item, value) => {
      const n = item.getItemData()
      if (n?.kind === 'connection') void renameConnection(n.connection, value)
      else if (n?.kind === 'folder') void renameFolder(n.folder, value)
    },
    scrollToItem: (item) => virtualizerRef.current?.scrollToIndex(item.getItemMeta().index, { align: 'auto' }),
    features: [syncDataLoaderFeature, selectionFeature, hotkeysCoreFeature, dragAndDropFeature, renamingFeature, expandAllFeature],
  })

  // Data changed: rebuild the flat list, drop stale selection / focus.
  useEffect(() => {
    tree.rebuildTree()
    const sel = tree.getState().selectedItems ?? []
    const keep = sel.filter((id) => index.nodes.has(id))
    if (keep.length !== sel.length) tree.setSelectedItems(keep)
    const focused = tree.getState().focusedItem
    if (focused && !index.nodes.has(focused)) tree.getItems()[0]?.setFocused()
  }, [index, tree])

  const items = tree.getItems()
  const virtualizer = useVirtualizer({
    count: items.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => rowHeight,
    overscan: 12,
    getItemKey: (i) => items[i]?.getId() ?? i,
  })
  virtualizerRef.current = virtualizer

  // Publish the focused node (commands like "New session" use its folder).
  const focusedId = tree.getState().focusedItem ?? null
  useEffect(() => {
    useTreeFocus.setState({ nodeId: focusedId })
  }, [focusedId])

  // --- reveal ----------------------------------------------------------------------------------------------------
  const pendingReveal = useRef<{ id: NodeId; until: number } | null>(null)
  const [revealTick, setRevealTick] = useState(0)
  useEffect(() => {
    const p = pendingReveal.current
    if (!p) return
    if (Date.now() > p.until) {
      pendingReveal.current = null
      return
    }
    if (!index.nodes.has(p.id)) return // wait for the data to arrive
    const ancestors: NodeId[] = []
    for (let cur = index.parent.get(p.id); cur && cur !== ROOT_ID; cur = index.parent.get(cur)) ancestors.push(cur)
    const missing = ancestors.filter((a) => !expandedIds.includes(a))
    if (missing.length) {
      setExpanded([...expandedIds, ...missing])
      return
    }
    const list = tree.getItems()
    const i = list.findIndex((it) => it.getId() === p.id)
    if (i < 0) return
    pendingReveal.current = null
    tree.setSelectedItems([p.id])
    list[i].setFocused()
    virtualizer.scrollToIndex(i, { align: 'center' })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [revealTick, index, expandedIds])

  useImperativeHandle(
    handleRef,
    (): TreeHandle => ({
      reveal(nodeId) {
        pendingReveal.current = { id: nodeId, until: Date.now() + 5000 }
        setRevealTick((t) => t + 1)
      },
      startRename(nodeId) {
        const item = tree.getItems().find((it) => it.getId() === nodeId)
        if (!item || !item.canRename()) return false
        item.setFocused()
        tree.setSelectedItems([nodeId])
        virtualizerRef.current?.scrollToIndex(item.getItemMeta().index, { align: 'auto' })
        item.startRenaming()
        return true
      },
      expandAll() {
        setExpanded(allFolders)
      },
      collapseAll() {
        setExpanded([])
      },
      focus() {
        tree.updateDomFocus()
      },
    }),
  )

  // --- interaction ---------------------------------------------------------------------------------------------
  const contextFor = (ids: string[], clicked?: NodeId): SessionMenuContext => {
    const connIds = ids.filter((id) => id.startsWith('c:')).map((id) => id.slice(2))
    const folderIds = ids.filter((id) => id.startsWith('f:')).map((id) => id.slice(2))
    const clickedNode = clicked ? index.nodes.get(clicked) : undefined
    return {
      connection: ids.length === 1 && clickedNode?.kind === 'connection' ? clickedNode.connection : undefined,
      folderId: ids.length === 1 && clickedNode?.kind === 'folder' ? clickedNode.folder.id : undefined,
      selection: connIds,
      folderIds,
    }
  }

  const selectedNodes = (): TreeNode[] => {
    const sel = tree.getState().selectedItems ?? []
    const ids = sel.length ? sel : tree.getState().focusedItem ? [tree.getState().focusedItem as string] : []
    return ids.map((id) => index.nodes.get(id)).filter((n): n is TreeNode => !!n)
  }

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (tree.isRenamingItem() || isEditableTarget(e.target)) return
    const focused = tree.getFocusedItem()
    const node = focused?.getItemData()
    if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey) {
      if (!node) return
      e.preventDefault()
      if (e.altKey) {
        if (node.kind === 'connection') openSessionEditor({ mode: 'edit', connectionId: node.connection.id })
        else if (node.kind === 'folder') void openFolderDialog({ mode: 'edit', folderId: node.folder.id })
        return
      }
      if (node.kind === 'connection') void connectSafely(node.connection)
      else if (node.kind === 'folder') {
        if (focused.isExpanded()) focused.collapse()
        else focused.expand()
      }
      return
    }
    if (e.key === 'Delete' || (e.key === 'Backspace' && (e.metaKey || e.ctrlKey))) {
      const nodes = selectedNodes().filter((n) => canModify(ownerOf(n), user))
      if (!nodes.length) return
      e.preventDefault()
      const conns = nodes.filter((n): n is Extract<TreeNode, { kind: 'connection' }> => n.kind === 'connection').map((n) => n.connection)
      const folders = nodes.filter((n): n is Extract<TreeNode, { kind: 'folder' }> => n.kind === 'folder').map((n) => n.folder)
      void (async () => {
        if (conns.length && !(await deleteConnections(conns))) return
        for (const f of folders) if (!(await deleteFolder(f))) return
      })()
      return
    }
    if ((e.key === 'a' || e.key === 'A') && e.metaKey && !e.ctrlKey) {
      // Cmd+A on macOS (headless-tree binds Control+A).
      e.preventDefault()
      tree.setSelectedItems(tree.getItems().map((i) => i.getId()))
      return
    }
    if (e.key.length === 1 && e.key !== ' ' && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault()
      onTypeToSearch(e.key)
    }
  }

  const containerProps = tree.getContainerProps('Saved sessions')

  return (
    <div
      ref={scrollRef}
      className="h-full overflow-x-hidden overflow-y-auto overscroll-contain"
      onContextMenu={(e: MouseEvent) => {
        if (!(e.target as Element).closest('[data-node-id]')) onContextTarget({ area: 'root', selection: [], folderIds: [] })
      }}
    >
      <div
        {...containerProps}
        aria-multiselectable="true"
        onKeyDown={onKeyDown}
        className="outline-none"
        style={{ ...(containerProps.style as CSSProperties | undefined), position: 'relative', height: virtualizer.getTotalSize(), minHeight: '100%' }}
      >
        {virtualizer.getVirtualItems().map((v) => {
          const item = items[v.index]
          if (!item) return null
          return (
            <TreeRow
              key={item.getId()}
              item={item}
              index={index}
              running={running}
              rowHeight={rowHeight}
              // Never animate row geometry (a global reduced-motion rule sets tiny transitions on everything).
              style={{ position: 'absolute', top: 0, left: 0, right: 0, height: rowHeight, transform: `translateY(${v.start}px)`, transitionProperty: 'none' }}
              onContext={(clicked) => {
                const sel = tree.getState().selectedItems ?? []
                if (!sel.includes(clicked)) {
                  tree.setSelectedItems([clicked])
                  item.setFocused()
                }
                onContextTarget(contextFor(sel.includes(clicked) ? sel : [clicked], clicked))
              }}
            />
          )
        })}
        <div aria-hidden style={tree.getDragLineStyle(-1, 0) as CSSProperties} className="pointer-events-none h-0.5 rounded-full bg-primary" />
      </div>
    </div>
  )
}

function TreeRow({
  item,
  index,
  running,
  rowHeight,
  style,
  onContext,
}: {
  item: ItemInstance<TreeNode>
  index: TreeIndex
  running: Map<string, RuntimeSession[]>
  rowHeight: number
  style: CSSProperties
  onContext: (id: NodeId) => void
}) {
  const node = item.getItemData()
  if (!node || node.kind === 'missing' || node.kind === 'root') return null
  const meta = item.getItemMeta()
  const props = item.getProps()
  const id = item.getId()
  const selected = item.isSelected()
  const renaming = item.isRenaming()
  const dropInto = item.isUnorderedDragTarget()
  const isFolder = node.kind === 'folder'
  const expanded = isFolder && item.isExpanded()
  const conn: Connection | undefined = node.kind === 'connection' ? node.connection : undefined

  return (
    <div
      {...props}
      data-node-id={id}
      style={{ ...style, paddingLeft: meta.level * INDENT + 4 }}
      className={cn(
        'group/row flex cursor-default items-center gap-1.5 pr-2 text-base outline-none select-none',
        'hover:bg-sidebar-accent/70 focus-visible:ring-1 focus-visible:ring-ring focus-visible:ring-inset',
        selected && 'bg-primary/15 hover:bg-primary/20',
        dropInto && 'bg-primary/20 ring-1 ring-primary/60 ring-inset',
        rowHeight > 28 && 'text-md',
      )}
      onDoubleClick={() => {
        if (conn && !renaming) void connectSafely(conn)
      }}
      onMouseDown={(e) => {
        // Middle-click opens without starting browser auto-scroll.
        if (e.button === 1) e.preventDefault()
      }}
      onAuxClick={(e) => {
        if (e.button === 1 && conn) {
          e.preventDefault()
          void connectSafely(conn)
        }
      }}
      onContextMenu={() => onContext(id)}
    >
      {isFolder ? (
        <ChevronRight aria-hidden className={cn('size-3.5 shrink-0 text-muted-foreground transition-transform duration-100', expanded && 'rotate-90')} />
      ) : (
        <span aria-hidden className="w-3.5 shrink-0" />
      )}
      {renaming ? (
        <RenameInput item={item} />
      ) : node.kind === 'folder' ? (
        <>
          <FolderIcon icon={node.folder.icon} color={node.folder.color} open={expanded} className={node.folder.color ? undefined : 'text-muted-foreground'} />
          <span className="min-w-0 truncate">{node.folder.name}</span>
          <span className="ml-auto flex shrink-0 items-center gap-1 pl-1">
            {node.folder.shared && <Users aria-label="Shared folder" className="size-3 text-muted-foreground" />}
            <FolderCount count={index.counts.get(id) ?? 0} color={safeColor(node.folder.color)} />
          </span>
        </>
      ) : conn ? (
        <ConnectionLabel conn={conn} running={running.get(conn.id)} tags={selected ? 'always' : 'hover'} />
      ) : null}
    </div>
  )
}

function RenameInput({ item }: { item: ItemInstance<TreeNode> }) {
  const tree = item.getTree()
  const inputRef = useRef<HTMLInputElement>(null)
  const p = item.getRenameInputProps() as { value: string; onChange: (e: { target?: { value: string } }) => void }
  useEffect(() => {
    const el = inputRef.current
    el?.focus()
    el?.select()
  }, [])
  return (
    <input
      ref={inputRef}
      value={p.value}
      onChange={(e) => p.onChange(e)}
      // Clicking elsewhere keeps the new name (Escape cancels, Enter confirms via the tree hotkeys).
      onBlur={() => tree.completeRenaming()}
      onClick={(e) => e.stopPropagation()}
      onMouseDown={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      aria-label="New name"
      maxLength={200}
      spellCheck={false}
      className="h-[calc(100%-4px)] min-w-0 flex-1 rounded-sm border border-ring bg-background px-1 text-base outline-none"
    />
  )
}

/** Number of sessions in a folder (subtree); a coloured folder tints its badge with its colour. */
function FolderCount({ count, color }: { count: number; color?: string }) {
  if (!color) return <span className="text-2xs text-muted-foreground tabular-nums">{count}</span>
  return (
    <span
      className="min-w-4 rounded-full px-1 text-center text-2xs leading-4 text-foreground/80 tabular-nums"
      style={{ background: `color-mix(in oklab, ${color} 22%, transparent)` }}
    >
      {count}
    </span>
  )
}
