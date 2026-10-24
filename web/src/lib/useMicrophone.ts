import { useCallback, useEffect, useState } from 'react'
import { MicError, openMicrophone, type MicSession, type MicState } from './recorder'

export type MicStatus = 'starting' | MicState

// useMicrophone owns the microphone: one session is opened and kept alive so
// the first capture is instant, and a retry replaces it rather than stacking a
// second stream on top of the first.
export function useMicrophone() {
  const [status, setStatus] = useState<MicStatus>('starting')
  const [note, setNote] = useState<string | null>(null)
  const [session, setSession] = useState<MicSession | null>(null)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
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
    return () => {
      live = false
    }
  }, [attempt])

  // One live stream at a time: a session that is replaced or unmounted is
  // released here, which is what turns the browser's recording indicator off.
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