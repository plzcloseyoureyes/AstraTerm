/* Always-mounted host of the webproxy dialogs (state in store.ts, so commands and menus can open them). */
import { ManagerDialog } from './ManagerDialog'
import { OpenDialog } from './OpenDialog'
import { closeWebDialog, closeXpraDialog, setManagerOpen, useWebproxyDialogs } from './store'
import { XpraDialog } from './XpraDialog'

export function WebproxyOverlay() {
  const { open, xpra, manager } = useWebproxyDialogs()
  return (
    <>
      {open && <OpenDialog init={open} onClose={closeWebDialog} />}
      {xpra && <XpraDialog init={xpra} onClose={closeXpraDialog} />}
      {manager && <ManagerDialog onClose={() => setManagerOpen(false)} />}
    </>
  )
}
