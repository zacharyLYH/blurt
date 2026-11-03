import { useCallback, useEffect, useState } from 'react'
import { MicError, openMicrophone, type MicSession, type MicState } from './recorder'

export type MicStatus = 'starting' | MicState

// useMicrophone owns the microphone: a stream is opened while `active` and
// released the moment it goes false. A live stream is a live microphone — the
// browser says so in the tab — so nothing may hold one after the capture screen
// is dismissed, and a retry replaces the session rather than stacking a second
// stream on top of the first.
export function useMicrophone(active = true) {
  const [status, setStatus] = useState<MicStatus>('starting')
  const [note, setNote] = useState<string | null>(null)
  const [session, setSession] = useState<MicSession | null>(null)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    if (!active) return
    let live = true
    openMicrophone()
      .then((opened) => {
        if (!live) {
          opened.close()
          return
        }
        setSession(opened)
        setStatus('ready')
      })
      .catch((err: unknown) => {
        if (!live) return
        setStatus(err instanceof MicError ? err.state : 'unavailable')
        setNote(err instanceof Error ? err.message : 'Microphone unavailable.')
      })
    // Leaving the screen, or closing the capture UI, releases the mic here:
    // this is what turns the browser's recording indicator off.
    return () => {
      live = false
      setSession(null)
    }
  }, [attempt, active])

  // One live stream at a time: a session that is replaced or unmounted is
  // released here.
  useEffect(() => {
    if (!session) return
    return () => session.close()
  }, [session])

  const retry = useCallback(() => {
    setStatus('starting')
    setNote(null)
    // Dropped first so nothing can start a capture on the stream being closed.
    setSession(null)
    setAttempt((count) => count + 1)
  }, [])

  return { status, note, session, retry }
}