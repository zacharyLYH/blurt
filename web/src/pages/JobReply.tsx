import { useNavigate, useParams } from 'react-router-dom'
import { Boom } from '@/components/Boom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { replyWithAudio } from '@/lib/jobs'
import { useMicrophone } from '@/lib/useMicrophone'
import { MAX_RECORDING_MS, useRecorder } from '@/lib/useRecorder'

// The voice screen for answering one Job out loud. It is the same control and
// the same recorder the home screen opens on, so a spoken reply is the capture
// experience pointed at a conversation that already exists.
//
// The screen is a route rather than a mode inside the thread, which is what
// makes the browser's own back gesture the way out of it: no custom reverse
// gesture, and nothing fighting the one the platform already owns.
export default function JobReply() {
  const navigate = useNavigate()
  const { jobId } = useParams()
  const id = Number(jobId)
  const { status, note, session, retry } = useMicrophone()

  // Back to the thread, wherever the user came from and however they leave:
  // a cancelled clip and a sent one land in the same place, and so does a
  // deep link that arrived here directly.
  const back = () => navigate(`/jobs/${id}`, { replace: true })

  const recorder = useRecorder(
    session,
    async (recording) => {
      await replyWithAudio(id, recording)
      back()
    },
    true,
  )
  const { state, recorded } = recorder
  const failed = state.kind === 'failed' ? state : null

  return (
    <main
      data-testid="voice-screen"
      className="fixed inset-0 flex flex-col items-center justify-center gap-5 bg-background px-6 py-8"
    >
      <h1 data-testid="reply-heading" className="sr-only">
        Answer out loud
      </h1>

      <Boom
        fullscreen
        session={session}
        recording={state.kind === 'recording'}
        remainingMs={MAX_RECORDING_MS - recorded}
        onRecord={recorder.start}
        onCancel={() => void recorder.cancel().then(back)}
        onSend={() => void recorder.send()}
      />

      {state.kind === 'sending' && (
        <p data-testid="reply-status" className="text-sm text-muted-foreground">
          Sending…
        </p>
      )}

      {failed && (
        <div className="flex w-full max-w-sm flex-col items-center gap-3">
          <Alert variant="destructive" data-testid="reply-error">
            <AlertDescription className="text-destructive">{failed.message}</AlertDescription>
          </Alert>
          <div className="flex gap-2">
            {failed.recording && (
              <Button type="button" data-testid="reply-retry" onClick={() => void recorder.retry()}>
                Retry upload
              </Button>
            )}
            <Button type="button" variant="ghost" onClick={back}>
              Type instead
            </Button>
          </div>
        </div>
      )}

      {status !== 'ready' && (
        <div className="flex flex-col items-center gap-2">
          <p data-testid="reply-mic-note" className="text-center text-xs text-muted-foreground">
            {note ?? 'Microphone unavailable.'}
          </p>
          {status !== 'starting' && (
            <Button type="button" variant="ghost" size="sm" onClick={retry}>
              Try mic again
            </Button>
          )}
        </div>
      )}
    </main>
  )
}
