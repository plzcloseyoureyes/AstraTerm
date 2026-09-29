/*
 * The term-transfer overlay (registerOverlay): one React subtree in the app shell that portals each terminal's layer
 * into its pane (an element the terminal plugin created there — it moves with the tab into pop-out windows) and
 * renders the feature's dialogs.
 */
import { createPortal } from 'react-dom'
import { useTransferStore } from '../store'
import { ConflictDialog, DangerDialog } from './Dialogs'
import { SendFileDialog } from './SendFileDialog'
import { TerminalLayer } from './TerminalLayer'

export function TermTransferOverlay({ locked }: { locked: boolean }) {
  const terms = useTransferStore((s) => s.terms)
  if (locked) return null
  return (
    <>
      {Object.values(terms).map((ui) =>
        ui.prompt || ui.transfer || ui.drag || ui.follower ? createPortal(<TerminalLayer ui={ui} />, ui.mount, ui.tabId) : null,
      )}
      <SendFileDialog />
      <ConflictDialog />
      <DangerDialog />
    </>
  )
}
