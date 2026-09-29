/*
 * Always-mounted overlay (registerOverlay) rendering the keys dialogs from the dialog store; each dialog is a lazy
 * chunk. The overlay unmounts while the app is locked, so typed passphrases do not linger behind the lock screen.
 */
import { lazy, Suspense } from 'react'
import { closeKeysDialog, useKeysDialogs } from './store'

const GenerateDialog = lazy(() => import('./dialogs/GenerateDialog'))
const ImportDialog = lazy(() => import('./dialogs/ImportDialog'))
const ExportDialog = lazy(() => import('./dialogs/ExportDialog'))
const InstallDialog = lazy(() => import('./dialogs/InstallDialog'))
const CertificateDialog = lazy(() => import('./dialogs/CertificateDialog'))
const SignDialog = lazy(() => import('./dialogs/SignDialog'))
const PassphraseDialog = lazy(() => import('./dialogs/PassphraseDialog'))
const IdentityDialog = lazy(() => import('./dialogs/IdentityDialog'))
const KnownHostDialog = lazy(() => import('./dialogs/KnownHostDialogs').then((m) => ({ default: m.KnownHostDialog })))
const MarkerDialog = lazy(() => import('./dialogs/KnownHostDialogs').then((m) => ({ default: m.MarkerDialog })))
const KnownHostsImportDialog = lazy(() => import('./dialogs/KnownHostDialogs').then((m) => ({ default: m.KnownHostsImportDialog })))

export function KeysOverlay() {
  const d = useKeysDialogs()
  return (
    <Suspense fallback={null}>
      {d.generate && <GenerateDialog key={d.generate.key} onClose={() => closeKeysDialog('generate')} />}
      {d.importer && <ImportDialog key={d.importer.key} mode={d.importer.mode} initialText={d.importer.text} onClose={() => closeKeysDialog('importer')} />}
      {d.exporter && <ExportDialog key={d.exporter.key} keyId={d.exporter.keyId} initialFormat={d.exporter.format} onClose={() => closeKeysDialog('exporter')} />}
      {d.install && <InstallDialog key={d.install.key} keyId={d.install.keyId} connectionId={d.install.connectionId} onClose={() => closeKeysDialog('install')} />}
      {d.certificate && <CertificateDialog key={d.certificate.key} keyId={d.certificate.keyId} onClose={() => closeKeysDialog('certificate')} />}
      {d.sign && <SignDialog key={d.sign.key} caKeyId={d.sign.caKeyId} subjectKeyId={d.sign.subjectKeyId} onClose={() => closeKeysDialog('sign')} />}
      {d.passphrase && <PassphraseDialog key={d.passphrase.key} keyId={d.passphrase.keyId} onClose={() => closeKeysDialog('passphrase')} />}
      {d.identity && <IdentityDialog key={d.identity.key} id={d.identity.id} initial={d.identity.initial} onClose={() => closeKeysDialog('identity')} />}
      {d.knownHost?.kind === 'host' && <KnownHostDialog key={d.knownHost.key} onClose={() => closeKeysDialog('knownHost')} />}
      {d.knownHost?.kind === 'marker' && (
        <MarkerDialog key={d.knownHost.key} marker={d.knownHost.marker} keyId={d.knownHost.keyId} onClose={() => closeKeysDialog('knownHost')} />
      )}
      {d.knownHostsImport && <KnownHostsImportDialog key={d.knownHostsImport.key} onClose={() => closeKeysDialog('knownHostsImport')} />}
    </Suspense>
  )
}
