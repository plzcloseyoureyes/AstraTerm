/*
 * Always-mounted overlay of the recordings feature: the share dialog and the admin "message the user" dialog.
 */
import { useState } from 'react'
import { MessageSquare } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Textarea } from '@/components/ui/textarea'
import { errorMessage } from '@/lib/utils'
import { messageSession } from './api'
import { ShareDialog } from './ShareDialog'
import { closeMessageDialog, useRecordingsUI } from './store'

export function RecordingsOverlay({ locked }: { locked: boolean }) {
  return (
    <>
      <ShareDialog locked={locked} />
      <MessageDialog locked={locked} />
    </>
  )
}

function MessageDialog({ locked }: { locked: boolean }) {
  const sessionId = useRecordingsUI((s) => s.messageSessionId)
  const title = useRecordingsUI((s) => s.messageTitle)
  const [text, setText] = useState('')
  const [sending, setSending] = useState(false)
  const send = async () => {
    if (!sessionId || !text.trim()) return
    setSending(true)
    try {
      await messageSession(sessionId, text.trim())
      toast.success('Message sent')
      setText('')
      closeMessageDialog()
    } catch (err) {
      toast.error('Could not send the message', { description: errorMessage(err) })
    } finally {
      setSending(false)
    }
  }
  return (
    <Dialog open={!!sessionId && !locked} onOpenChange={(v) => !v && closeMessageDialog()}>
      <DialogContent size="md">
        <DialogHeader>
          <DialogTitle>
            <MessageSquare className="size-4 text-muted-foreground" /> Message the user
          </DialogTitle>
          <DialogDescription>
            Shown in the session{title ? ` “${title}”` : ''} as a notice line and as a notification in the user’s AstraTerm windows.
          </DialogDescription>
        </DialogHeader>
        <Textarea
          autoFocus
          value={text}
          maxLength={500}
          rows={3}
          placeholder="e.g. The server restarts in 5 minutes — please save your work."
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void send()
          }}
        />
        <DialogFooter>
          <Button variant="secondary" onClick={closeMessageDialog}>
            Cancel
          </Button>
          <Button onClick={() => void send()} loading={sending} disabled={!text.trim()}>
            Send
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
