/*
 * Always-mounted AI overlay: the inline command bar and the run confirmation.
 */
import { useEffect } from 'react'
import { CommandBar } from './CommandBar'
import { closeCommandBar } from './barStore'
import { RunDialog } from './RunDialog'

export function AiOverlay({ locked }: { locked: boolean }) {
  useEffect(() => {
    if (locked) closeCommandBar(false)
  }, [locked])
  return (
    <>
      {!locked && <CommandBar />}
      <RunDialog locked={locked} />
    </>
  )
}
