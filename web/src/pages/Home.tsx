import { useCallback, useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Boom } from '@/components/Boom'
import { JobThread } from '@/components/JobThread'
import { TextComposer } from '@/components/TextComposer'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Item, ItemActions, ItemContent, ItemGroup, ItemTitle } from '@/components/ui/item'
import questionnaire from '@/data/questionnaire/v1.json'
import { api } from '@/lib/api'
import { listJobs, sendAudio, sendText, type Job, type JobSummary } from '@/lib/jobs'
import { setupPush, type PushSetup } from '@/lib/push'
import { useMicrophone, type MicStatus } from '@/lib/useMicrophone'
import { MAX_RECORDING_MS, useRecorder } from '@/lib/useRecorder'
import { useUser } from '@/lib/user'
import { cn, messageOf } from '@/lib/utils'

const pushLabel: Record<PushSetup, string> = {
  ready: 'Notifications on',
  pending: 'Notifications pending',
  denied: 'Notifications blocked',
  unsupported: 'Notifications unsupported',
}

const pushDot: Record<PushSetup, string> = {
  ready: 'bg-emerald-400',
  pending: 'bg-primary',
  denied: 'bg-destructive',
  unsupported: 'bg-muted-foreground',
}

const micLabel: Record<MicStatus, string> = {
  starting: 'Checking mic…',
  ready: 'Mic ready',
  denied: 'Mic blocked',
  unavailable: 'No mic',
  unsupported: 'Voice unsupported',
}

const micDot: Record<MicStatus, string> = {
  starting: 'bg-primary',
  ready: 'bg-emerald-400',
  denied: 'bg-destructive',
  unavailable: 'bg-muted-foreground',
  unsupported: 'bg-muted-foreground',
}

// The sidebar reads the same statuses the thread does, in the user's words.
const jobLabel: Record<Job['status'], string> = {
  open: 'Working…',
  clarifying: 'Needs your answer',
  complete: 'Done',
  failed: "Didn't catch that",
}

// Failing is the only status worth colour: everything else is still in flight,
// which the user already knows.
const jobVariant: Record<Job['status'], 'default' | 'secondary' | 'destructive' | 'outline'> = {
  open: 'secondary',
  clarifying: 'default',
  complete: 'outline',
  failed: 'destructive',
}

export default function Home() {
  const navigate = useNavigate()
  const { user, setUser } = useUser()
  const { status: mic, note: micNote, session, retry: retryMic } = useMicrophone()
  const [push, setPush] = useState<PushSetup>('pending')
  const [thread, setThread] = useState<Job | null>(null)
  const [open, setOpen] = useState<JobSummary[]>([])

  // Setup is not done until the current questionnaire version is finished.
  useEffect(() => {
    if (user && user.lastQuestionnaireVersion < questionnaire.version) {
      navigate('/questionnaire', { replace: true })
    }
  }, [user, navigate])

  // Retry the push handoff on every load: the browser subscription may already
  // exist from a previous visit while the server was not ready.
  useEffect(() => {
    let live = true
    void setupPush({ prompt: false }).then((state) => {
      if (live) setPush(state)
    })
    return () => {
      live = false
    }
  }, [])

  // The sidebar is the only way back to a capture. It refreshes on the events that
  // can change it — app entry, a new capture, a thread settling — instead of on a
  // timer: with no agent, nothing else moves a Job.
  const refreshOpen = useCallback(() => {
    void listJobs()
      .then(setOpen)
      .catch((err: unknown) => console.warn('Blurt: open jobs failed', err))
  }, [])

  useEffect(() => {
    refreshOpen()
  }, [refreshOpen])

  // One clip at a time, and the boom opens the screen: as soon as the mic is
  // live the recorder starts, and cancelling opts out until the next capture.
  // The same hook runs a Job's voice reply, so the lifecycle exists once.
  const recorder = useRecorder(
    session,
    async (recording) => {
      setThread(await sendAudio(recording))
      // Show it in the sidebar now rather than up to a poll later.
      refreshOpen()
    },
    true,
  )

  // The draft only leaves the composer once the server has the Job.
  async function submitText(value: string): Promise<boolean> {
    try {
      setThread(await sendText(value))
      // Show it in the sidebar now rather than up to a poll later.
      refreshOpen()
      return true
    } catch (err) {
      recorder.fail(messageOf(err))
      return false
    }
  }

  // A new capture: drop the thread and put the boom back in front of the user.
  function resetCapture() {
    setThread(null)
    recorder.reset()
  }

  async function signOut() {
    try {
      await api<void>('/api/auth/logout', { method: 'POST' })
    } catch (err) {
      // The local session is gone either way; a failed revoke must not trap
      // the user on Home.
      console.warn('Blurt: logout request failed', err)
    } finally {
      setUser(null)
      navigate('/login', { replace: true })
    }
  }

  const failed = recorder.state.kind === 'failed' ? recorder.state : null

  return (
    <div className="flex min-h-dvh flex-col md:grid md:grid-cols-[270px_1fr]">
      <aside className="flex items-center gap-3 border-b border-border bg-black/20 px-4 py-3 md:flex-col md:items-stretch md:gap-2 md:border-b-0 md:border-r md:p-5">
        <div className="flex items-center gap-2 md:mb-4">
          <div className="relative size-7 rounded-lg bg-gradient-to-br from-[#ffcb6b] to-[#d9861c]">
            <span className="absolute inset-[27%_37%] rounded-full bg-[#241a08]" />
          </div>
          <span className="font-bold tracking-wide">Blurt</span>
        </div>
        <h2 className="hidden text-xs uppercase tracking-[0.12em] text-muted-foreground md:block">Waiting on you</h2>
        {open.length === 0 ? (
          <p className="hidden text-sm text-muted-foreground md:block">Clarification threads show up here.</p>
        ) : (
          // Mobile keeps the same list as a horizontal strip: the sidebar is
          // just a top bar there, and hiding it would leave nothing to tap.
          <ItemGroup
            data-testid="open-jobs"
            aria-label="Open conversations"
            className="min-w-0 flex-1 flex-row gap-2 overflow-x-auto md:min-h-0 md:flex-col md:overflow-y-auto"
          >
            {open.map((job) => (
              <Item
                key={job.jobId}
                asChild
                variant="outline"
                size="sm"
                className="max-w-[13rem] shrink-0 md:max-w-none"
              >
                <Link to={`/jobs/${job.jobId}`} data-testid="open-job">
                  <ItemContent>
                    <ItemTitle className="w-full truncate">{job.preview || 'Voice clip'}</ItemTitle>
                  </ItemContent>
                  <ItemActions>
                    <Badge variant={jobVariant[job.status]}>{jobLabel[job.status]}</Badge>
                  </ItemActions>
                </Link>
              </Item>
            ))}
          </ItemGroup>
        )}
        <div
          data-testid="push-status"
          className="ml-auto flex items-center gap-2 text-xs text-muted-foreground md:ml-0 md:mt-auto md:border-t md:border-border md:pt-4"
        >
          <span className={cn('size-2 rounded-full', pushDot[push])} aria-hidden="true" />
          {pushLabel[push]}
        </div>
      </aside>

      <main className="flex flex-col gap-8 px-4 py-6 md:px-10 md:py-8">
        <header className="flex items-start justify-between gap-4">
          <div>
            <h1 data-testid="home-heading" className="text-2xl font-semibold tracking-tight md:text-3xl">
              Hi {user?.name || 'there'}
            </h1>
            <p className="mt-1 text-sm text-muted-foreground">{user?.email}</p>
          </div>
          <Button variant="ghost" size="sm" onClick={() => void signOut()}>
            Sign out
          </Button>
        </header>

        <section className="my-auto flex w-full max-w-xl flex-col gap-5">
          <div>
            <p className="text-xs uppercase tracking-[0.16em] text-primary">Ready when you are</p>
            <h2 className="text-2xl font-semibold tracking-tight md:text-4xl">Tell Blurt what to remember.</h2>
          </div>

          <div className="flex items-center justify-between gap-2">
            <div data-testid="mic-status" className="flex items-center gap-2 text-xs text-muted-foreground">
              <span className={cn('size-2 rounded-full', micDot[mic])} aria-hidden="true" />
              {micLabel[mic]}
            </div>
            {(mic === 'denied' || mic === 'unavailable' || mic === 'unsupported') && (
              <Button type="button" variant="ghost" size="sm" onClick={retryMic}>
                Try mic again
              </Button>
            )}
          </div>

          <Card className="items-center gap-4 bg-card/80 p-5">
            {thread ? (
              <JobThread job={thread} onReset={resetCapture} onSettled={refreshOpen} />
            ) : (
              <Boom
                session={session}
                recording={recorder.state.kind === 'recording'}
                remainingMs={MAX_RECORDING_MS - recorder.recorded}
                onRecord={recorder.start}
                onCancel={() => void recorder.cancel()}
                onSend={() => void recorder.send()}
              />
            )}

            {recorder.state.kind === 'sending' && (
              <p data-testid="capture-status" className="text-sm text-muted-foreground">
                Sending…
              </p>
            )}

            {failed && (
              <div className="flex w-full flex-col items-center gap-2">
                <Alert variant="destructive" data-testid="capture-error">
                  <AlertDescription className="text-destructive">{failed.message}</AlertDescription>
                </Alert>
                {failed.recording && (
                  <Button type="button" data-testid="retry-upload" onClick={() => void recorder.retry()}>
                    Retry upload
                  </Button>
                )}
              </div>
            )}
          </Card>

          <TextComposer disabled={recorder.state.kind === 'recording'} focus={recorder.landed} onSend={submitText} />

          <p data-testid="mic-note" className="text-xs text-muted-foreground">
            {micNote ?? 'Audio is WAV, 16 kHz, mono. The file is deleted after Blurt transcribes it.'}
          </p>
        </section>
      </main>
    </div>
  )
}