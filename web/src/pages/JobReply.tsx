import { useNavigate, useParams } from 'react-router-dom'
import { Boom } from '@/components/Boom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { replyWithAudio } from '@/lib/jobs'
import { useMicrophone } from '@/lib/useMicrophone'
import { MAX_RECORDING_MS, useRecorder } from '@/lib/useRecorder'

// The boom on its own screen, for answering one Job out loud. It is the same
// control and the same recorder the home screen opens on, so a spoken reply is
// the capture experience pointed at a conversation that already exists.
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
    <main className="grid min-h-dvh place-items-center px-4 py-8">
      <Card className="w-full max-w-xl items-center gap-3 bg-card/80 p-5">
        <CardHeader className="items-center text-center">
          <CardTitle data-testid="reply-heading">Answer out loud</CardTitle>
          <CardDescription>Blurt is listening. Cancel to drop the clip, or send it as your answer.</CardDescription>
        </CardHeader>

        <Boom
          session={session}
          recording={state.kind === 'recording'}
          remainingMs={MAX_RECORDING_MS - recorded}
          onRecord={recorder.start}
          onCancel={() => void recorder.cancel().then(back)}
          onSend={() => void recorder.send()}
        />

        {state.kind === 'sending' && (
          <CardDescription data-testid="reply-status">Sending…</CardDescription>
        )}

        {failed && (
          <div className="flex w-full flex-col items-center gap-3">
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
            <CardDescription data-testid="reply-mic-note" className="text-xs">
              {note ?? 'Microphone unavailable.'}
            </CardDescription>
            {status !== 'starting' && (
              <Button type="button" variant="ghost" size="sm" onClick={retry}>
                Try mic again
              </Button>
            )}
          </div>
        )}
      </Card>
    </main>
  )
}