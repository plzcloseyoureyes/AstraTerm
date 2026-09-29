/*
 * The files feature's overlay (registerOverlay): the transfer queue drawer and every dialog opened through
 * ./store. Heavy dialogs (preview with the image viewer and Markdown, search, folder compare) load on demand.
 */
import { lazy, Suspense, useEffect, useRef } from 'react'
import { getActiveController, refocusAfterDialog } from '../browser/controller'
import { TransferDrawer } from '../transfers/TransferDrawer'
import { ConflictDialog } from './ConflictDialog'
import { ChecksumDialog, CompressDialog, PropertiesDialog } from './InfoDialogs'
import { PermissionsDialog } from './PermissionsDialog'
import { closeFilesDialog, useFilesDialogs, type OpenDialog } from './store'

const PreviewDialog = lazy(() => import('./PreviewDialog'))
const SearchDialog = lazy(() => import('./SearchDialog'))
const CompareDialog = lazy(() => import('./CompareDialog'))

function DialogFor({ d }: { d: OpenDialog }) {
  const close = () => closeFilesDialog(d.id)
  switch (d.kind) {
    case 'conflict':
      return <ConflictDialog names={d.names} destLabel={d.destLabel} verb={d.verb} onDone={d.resolve} />
    case 'permissions':
      return <PermissionsDialog ctx={d.ctx} entries={d.entries} onClose={close} />
    case 'properties':
      return <PropertiesDialog ctx={d.ctx} entry={d.entry} onClose={close} />
    case 'checksum':
      return <ChecksumDialog ctx={d.ctx} entry={d.entry} onClose={close} />
    case 'compress':
      return <CompressDialog ctx={d.ctx} entries={d.entries} dir={d.dir} onClose={close} />
    case 'preview':
      return <PreviewDialog ctx={d.ctx} entry={d.entry} siblings={d.siblings} onClose={close} />
    case 'search':
      return <SearchDialog ctx={d.ctx} path={d.path} viewId={d.viewId} onClose={close} />
    case 'compare':
      return <CompareDialog left={d.left} right={d.right} onClose={close} />
    default:
      return null
  }
}

export function FilesOverlay({ locked }: { locked: boolean }) {
  const stack = useFilesDialogs((s) => s.stack)
  // A dialog closed (they are opened programmatically, without a trigger to return the focus to): give the keyboard
  // back to the file list the user worked in.
  const depth = useRef(stack.length)
  useEffect(() => {
    if (stack.length < depth.current) refocusAfterDialog(() => getActiveController()?.focusList())
    depth.current = stack.length
  }, [stack.length])
  return (
    <>
      <TransferDrawer locked={locked} />
      {!locked &&
        stack.map((d) => (
          <Suspense key={d.id} fallback={null}>
            <DialogFor d={d} />
          </Suspense>
        ))}
    </>
  )
}
