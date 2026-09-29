/*
 * Folder create / edit dialog: name, parent folder, colour, icon, shared (administrators).
 */
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { Controller, useForm, type Resolver } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { Folder as FolderGlyph } from 'lucide-react'
import { useCreateFolder, useFolders, useUpdateFolder } from '@/api/folders'
import type { Folder, FolderInput } from '@/api/types'
import { Button } from '@/components/ui/button'
import { ColorSwatchPicker } from '@/components/ui/color-swatch-picker'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { SimpleSelect } from '@/components/ui/select'
import { LoadingPane } from '@/components/ui/spinner'
import { SwitchField } from '@/components/ui/switch'
import { errorMessage } from '@/lib/utils'
import { useCurrentUser } from '@/stores/auth'
import { closeFolderDialog, type FolderRequest } from './dialogs/store'
import { IconPicker } from './IconPicker'
import { FolderIcon, MAX_ICON_CHARS } from './icons'
import { canModify, flattenFolders, folderNodeId, isFolderInSubtree, nextSortOrder } from './model'
import { revealNode } from './panel/controller'

const ROOT = '__root__'

const schema = z.object({
  name: z.string().trim().min(1, 'Enter a folder name').max(200, 'At most 200 characters'),
  parentId: z.string().nullable(),
  color: z.string().max(64).nullable(),
  icon: z.string().max(MAX_ICON_CHARS, 'The icon image is too large').nullable(),
  shared: z.boolean(),
})

type FolderFormValues = z.infer<typeof schema>

export default function FolderDialog({ request, hidden }: { request: FolderRequest; hidden: boolean }) {
  const folders = useFolders()
  const original = request.mode === 'edit' ? folders.data?.find((f) => f.id === request.folderId) : undefined
  if (request.mode === 'edit' && !original) {
    return (
      <Dialog open={!hidden} onOpenChange={(o) => !o && closeFolderDialog(null)}>
        <DialogContent size="sm">
          <DialogHeader>
            <DialogTitle>Edit folder</DialogTitle>
            <DialogDescription>{folders.isLoading ? 'Loading…' : 'This folder no longer exists.'}</DialogDescription>
          </DialogHeader>
          {folders.isLoading ? (
            <LoadingPane />
          ) : (
            <DialogFooter>
              <Button onClick={() => closeFolderDialog(null)}>Close</Button>
            </DialogFooter>
          )}
        </DialogContent>
      </Dialog>
    )
  }
  return <FolderForm request={request} original={original} folders={folders.data ?? []} hidden={hidden} />
}

function FolderForm({ request, original, folders, hidden }: { request: FolderRequest; original?: Folder; folders: Folder[]; hidden: boolean }) {
  const user = useCurrentUser()
  const isAdmin = user?.role === 'admin'
  const readOnly = !!original && !canModify(original, user)
  const createFolder = useCreateFolder()
  const updateFolder = useUpdateFolder()
  const [submitError, setSubmitError] = useState<string | null>(null)
  const ids = { parent: useId(), color: useId(), icon: useId() }
  const nameRef = useRef<HTMLInputElement | null>(null)
  const colorRef = useRef<HTMLDivElement>(null)

  const form = useForm<FolderFormValues>({
    defaultValues: {
      name: original?.name ?? '',
      parentId: original ? original.parentId || null : request.parentId || null,
      color: original?.color || null,
      icon: original?.icon || null,
      shared: original?.shared ?? false,
    },
    resolver: zodResolver(schema) as unknown as Resolver<FolderFormValues>,
  })
  const { register, control, handleSubmit, formState, watch } = form
  const color = watch('color')
  const nameField = register('name')

  // Parents: every folder except this one and its descendants (no cycles).
  const parentOptions = useMemo(() => {
    const byId = new Map(folders.map((f) => [f.id, f]))
    const flat = flattenFolders(folders).filter((f) => !original || !isFolderInSubtree(byId, original.id, f.folder.id))
    return [
      { value: ROOT, label: 'Top level' },
      ...flat.map((f) => ({ value: f.folder.id, label: <span style={{ paddingLeft: f.depth * 12 }}>{f.folder.name}</span> })),
    ]
  }, [folders, original])

  useEffect(() => {
    if (hidden) return
    const t = setTimeout(() => {
      if (request.focus === 'color') colorRef.current?.querySelector<HTMLElement>('[role=radio]')?.focus()
      else if (request.focus !== 'icon') {
        nameRef.current?.focus()
        nameRef.current?.select()
      }
    }, 30)
    return () => clearTimeout(t)
  }, [hidden, request.focus])

  const onSubmit = handleSubmit(async (v) => {
    if (readOnly) return
    setSubmitError(null)
    try {
      let saved: Folder
      if (original) {
        const patch: Partial<FolderInput> = {}
        if (v.name !== original.name) patch.name = v.name
        if ((v.parentId || null) !== (original.parentId || null)) {
          patch.parentId = v.parentId
          patch.sortOrder = nextSortOrder(folders.filter((f) => (f.parentId || null) === (v.parentId || null) && f.id !== original.id))
        }
        if ((v.color || '') !== (original.color || '')) patch.color = v.color ?? ''
        if ((v.icon || '') !== (original.icon || '')) patch.icon = v.icon ?? ''
        if (v.shared !== original.shared) patch.shared = v.shared
        saved = Object.keys(patch).length ? await updateFolder.mutateAsync({ id: original.id, patch }) : original
      } else {
        saved = await createFolder.mutateAsync({
          name: v.name,
          parentId: v.parentId,
          color: v.color ?? '',
          icon: v.icon ?? '',
          shared: v.shared,
          sortOrder: nextSortOrder(folders.filter((f) => (f.parentId || null) === (v.parentId || null))),
        })
        revealNode(folderNodeId(saved.id))
      }
      toast.success(original ? `Folder "${saved.name}" saved` : `Folder "${saved.name}" created`)
      closeFolderDialog(saved)
    } catch (err) {
      setSubmitError(errorMessage(err))
    }
  })

  const canShare = isAdmin || (!!original?.shared && original.ownerId === user?.id)

  return (
    <Dialog open={!hidden} onOpenChange={(o) => !o && closeFolderDialog(null)}>
      <DialogContent size="md" onOpenAutoFocus={(e) => e.preventDefault()}>
        <form onSubmit={onSubmit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>
              <FolderIcon icon={form.watch('icon')} color={color} className="text-muted-foreground" />
              {original ? (readOnly ? `Folder “${original.name}”` : 'Edit folder') : 'New folder'}
            </DialogTitle>
            <DialogDescription>{readOnly ? 'This folder is shared by another user and is read-only.' : 'Folders group sessions in the tree.'}</DialogDescription>
          </DialogHeader>

          <fieldset disabled={readOnly} className="m-0 grid min-w-0 gap-4 border-0 p-0">
            <Field label="Name" required error={formState.errors.name?.message}>
              <Input
                {...nameField}
                ref={(el) => {
                  nameField.ref(el)
                  nameRef.current = el
                }}
                placeholder="Production"
                autoComplete="off"
                maxLength={200}
              />
            </Field>
            <Field label="Parent folder" htmlFor={ids.parent}>
              <Controller
                control={control}
                name="parentId"
                render={({ field }) => (
                  <SimpleSelect id={ids.parent} value={field.value || ROOT} onValueChange={(v) => field.onChange(v === ROOT ? null : v)} options={parentOptions} />
                )}
              />
            </Field>
            <div ref={colorRef}>
              <Field label="Colour">
                <Controller
                  control={control}
                  name="color"
                  render={({ field }) => (
                    <ColorSwatchPicker value={field.value} onChange={(c) => field.onChange(c ?? null)} allowNone size="sm" aria-label="Folder colour" />
                  )}
                />
              </Field>
            </div>
            <Field label="Icon" htmlFor={ids.icon} error={formState.errors.icon?.message}>
              <Controller
                control={control}
                name="icon"
                render={({ field }) => <IconPicker id={ids.icon} value={field.value} onChange={field.onChange} fallback={FolderGlyph} color={color} />}
              />
            </Field>
            <Controller
              control={control}
              name="shared"
              render={({ field }) => (
                <SwitchField
                  label="Shared with all users"
                  description={canShare ? 'Everyone sees this folder; its sessions keep their own sharing.' : 'Only administrators can share folders.'}
                  checked={field.value}
                  disabled={!canShare}
                  onCheckedChange={field.onChange}
                />
              )}
            />
          </fieldset>

          {submitError && (
            <p role="alert" className="text-sm text-destructive">
              {submitError}
            </p>
          )}
          <DialogFooter>
            <Button variant="secondary" onClick={() => closeFolderDialog(null)}>
              {readOnly ? 'Close' : 'Cancel'}
            </Button>
            {!readOnly && (
              <Button type="submit" loading={formState.isSubmitting}>
                {original ? 'Save' : 'Create folder'}
              </Button>
            )}
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
