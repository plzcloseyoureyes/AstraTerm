/*
 * Always-mounted overlay (registerOverlay) rendering the import wizard and export dialog from the dialog store, each
 * a lazy chunk. Unmounted while the app is locked so a typed passphrase does not linger behind the lock screen.
 */
import { lazy, Suspense } from 'react'
import { closeImporterDialog, useImporterDialogs } from './store'

const ImportWizard = lazy(() => import('./ImportWizard'))
const ExportDialog = lazy(() => import('./ExportDialog'))

export function ImporterOverlay() {
  const d = useImporterDialogs()
  return (
    <Suspense fallback={null}>
      {d.wizard && <ImportWizard key={d.wizard.key} initialFormat={d.wizard.format} onClose={() => closeImporterDialog('wizard')} />}
      {d.export && (
        <ExportDialog
          key={d.export.key}
          initialFormat={d.export.format}
          folderId={d.export.folderId}
          connectionIds={d.export.connectionIds}
          onClose={() => closeImporterDialog('export')}
        />
      )}
    </Suspense>
  )
}
