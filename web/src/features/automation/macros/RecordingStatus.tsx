/*
 * Status bar indicator while a macro is being recorded (click to stop). Recording stops by itself when the recorded
 * terminal closes.
 */
import * as React from 'react'
import { Circle } from 'lucide-react'
import { useTerminals } from '@/features/terminal/bus'
import { StatusBarItem } from '@/layout/StatusBar'
import { stopRecording, useRecorder } from './recorder'

export function RecordingStatusItem() {
  const rec = useRecorder()
  const terminals = useTerminals()
  React.useEffect(() => {
    if (rec.recording && rec.tabId && !terminals.some((t) => t.tabId === rec.tabId)) stopRecording()
  }, [rec.recording, rec.tabId, terminals])
  if (!rec.recording) return null
  return (
    <StatusBarItem tone="danger" icon={Circle} onClick={stopRecording} tooltip={`Recording a macro in ${rec.title ?? 'a terminal'} — click to stop`} aria-label="Stop macro recording">
      REC · {rec.steps.length} steps
    </StatusBarItem>
  )
}
