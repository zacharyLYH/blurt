import { useEffect, useState } from 'react'
import type { MicSession, Recording } from './recorder'
import { messageOf } from './utils'

// The server caps a clip at one minute, and the gateway takes seconds to decode
// one, so the client stops a beat early rather than trusting a duration it
// reports to land under the cap.
export const MAX_RECORDING_MS = 59_000

// One clip's lifecycle: nothing captured, recording, being sent, or failed with
// the blob kept, because retrying an upload should not mean speaking again.
export type Recorder =
  | { kind: 'idle' }
  | { kind: 'recording' }
  | { kind: 'sending' }
  | { kind: 'failed'; message: string; recording?: Recording }

// useRecorder owns one clip for whichever screen is capturing: the recording
// clock, the cap, and the three moves a clip has. The home screen opens a Job
// with it and a Job's thread answers with it, so the capture lifecycle exists
// once rather than twice.
//
// `auto` starts recording the moment the mic is live, which is what makes the
// boom the first thing either screen shows. Cancelling, and finishing a send,
// both opt out; `landed` is how a caller knows to move focus to its own
// fallback, and `reset` brings the recorder back for another clip.
export function useRecorder(
  session: MicSession | null,
  onSent: (recording: Recording) => Promise<void>,
  auto: boolean,
) {
  const [state, setState] = useState<Recorder>({ kind: 'idle' })
  const [recorded, setRecorded] = useState(0)
  const [landed, setLanded] = useState(false)

  async function deliver(recording: Recording) {
    setState({ kind: 'sending' })
    try {
      await onSent(recording)
      // The screen is done with this clip. Landing here is what stops `auto`
      // from opening the boom again the moment the send finishes.
      setLanded(true)
      setState({ kind: 'idle' })
    } catch (err) {
      // Keep the blob: retrying should not mean speaking again.
      setState({ kind: 'failed', message: messageOf(err), recording })
    }
  }

  function start() {
    if (!session) return
    try {
      session.start()
      setRecorded(0)
      setLanded(false)
      setState({ kind: 'recording' })
    } catch (err) {
      setState({ kind: 'failed', message: messageOf(err) })
    }
  }

  // Cancel throws the clip away. The caller decides where the user lands; this
  // only reports that they opted out.
  async function cancel() {
    try {
      await session?.stop()
    } catch {
      // A cancelled clip is discarded whether or not the encoder liked it.
    }
    setLanded(true)
    setState({ kind: 'idle' })
  }

  async function send() {
    if (!session) return
    setState({ kind: 'sending' })
    let recording: Recording
    try {
      recording = await session.stop()
    } catch (err) {
      setState({ kind: 'failed', message: messageOf(err) })
      return
    }
    await deliver(recording)
  }

  async function retry() {
    if (state.kind === 'failed' && state.recording) await deliver(state.recording)
  }

  function reset() {
    setRecorded(0)
    setLanded(false)
    setState({ kind: 'idle' })
  }

  // fail lets the caller report its own delivery problem — a typed turn the
  // server refused, say — through the one error the screen already shows.
  function fail(message: string) {
    setState({ kind: 'failed', message })
  }

  // The recording clock doubles as the cap: one interval decides when the clip
  // has run out of time and hands it to the sender.
  useEffect(() => {
    if (state.kind !== 'recording') return
    const startedAt = performance.now()
    const timer = window.setInterval(() => {
      const elapsed = performance.now() - startedAt
      setRecorded(elapsed)
      if (elapsed >= MAX_RECORDING_MS) {
        window.clearInterval(timer)
        void send()
      }
    }, 200)
    return () => window.clearInterval(timer)
    // send is render-local; depending on it would restart the clock.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.kind])

  // The boom opens the screen: as soon as the mic is live, recording starts
  // without a tap. Opening the microphone is the external system this effect
  // exists to drive, and the recorder's state is its mirror — so the extra
  // render the rule warns about is the point, not a cascade.
  const micLive = session !== null
  useEffect(() => {
    if (!auto || !micLive || landed || state.kind !== 'idle') return
    // eslint-disable-next-line react-hooks/set-state-in-effect
    start()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auto, micLive, landed, state.kind])

  return { state, recorded, landed, start, cancel, send, retry, reset, fail }
}