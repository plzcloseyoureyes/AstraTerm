/*
 * Logon actions of one connection in a dialog (session tree context menu, command automation.logonActions {id}).
 */
import { LogIn } from 'lucide-react'
import { useConnection } from '@/api/connections'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { useAutomationUI } from '../store'
import { LogonEditor } from './LogonEditor'

export default function LogonEditorDialog() {
  const st = useAutomationUI((s) => s.logonEditor)
  const { data: conn } = useConnection(st?.connectionId)
  if (!st) return null
  const close = () => useAutomationUI.setState({ logonEditor: null })
  return (
    <Dialog open onOpenChange={(o) => !o && close()}>
      <DialogContent size="2xl" className="max-h-[88vh]">
        <DialogHeader>
          <DialogTitle>
            <LogIn className="size-4 text-primary" /> Logon actions{conn ? ` — ${conn.name}` : ''}
          </DialogTitle>
          <DialogDescription>Expect / send steps typed automatically after every connect.</DialogDescription>
        </DialogHeader>
        <div className="-mx-5 min-h-0 flex-1 overflow-y-auto px-5">
          <LogonEditor connectionId={st.connectionId} onSaved={close} onCancel={close} />
        </div>
      </DialogContent>
    </Dialog>
  )
}
