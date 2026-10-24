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
import type { Recording } from '@/lib/recorder'
import { useMicrophone, type MicStatus } from '@/lib/useMicrophone'
import { useUser } from '@/lib/user'
import { cn } from '@/lib/utils'

// The server caps a clip at one minute, and the gateway takes seconds to decode
// one, so the client stops a beat early rather than trusting a duration it
// reports to land under the cap.
const MAX_RECORDING_MS = 59_000

// One capture at a time. The composer stays mounted throughout, so a failed
// upload never costs the user the words they typed.
type Capture =
  | { kind: 'idle' }
  | { kind: 'recording' }
  | { kind: 'sending' }
  | { kind: 'failed'; message: string; recording?: Recording }
  | { kind: 'thread'; job: Job }

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
  queued: 'Waiting',
  processing: 'Transcribing…',
  clarifying: 'Needs your answer',
  complete: 'Done',
  failed: "Didn't catch that",
}

// Failing is the only status worth colour: everything else is still in flight,
// which the user already knows.
const jobVariant: Record<Job['status'], 'default' | 'secondary' | 'destructive' | 'outline'> = {
  queued: 'secondary',
  processing: 'secondary',
  clarifying: 'default',
  complete: 'outline',
  failed: 'destructive',
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : 'Something went wrong.'
}

export default function Home() {
  const navigate = useNavigate()
  const { user, setUser } = useUser()
  const { status: mic, note: micNote, session, retry: retryMic } = useMicrophone()
  const [push, setPush] = useState<PushSetup>('pending')
  const [capture, setCapture] = useState<Capture>({ kind: 'idle' })
  const [recorded, setRecorded] = useState(0)
  const [landed, setLanded] = useState(false)
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

  // The recording clock doubles as the cap: one interval decides when the clip
  // has run out of time and hands it to the sender.
  useEffect(() => {
    if (capture.kind !== 'recording') return
    const startedAt = performance.now()
    const timer = window.setInterval(() => {
      const elapsed = performance.now() - startedAt
      setRecorded(elapsed)
      if (elapsed >= MAX_RECORDING_MS) {
        window.clearInterval(timer)
        void sendRecording()
      }
    }, 200)
    return () => window.clearInterval(timer)
    // sendRecording is render-local; depending on it would restart the clock.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [capture.kind])

  // The app opens on the boom: as soon as the mic is live, recording starts
  // without a tap. Cancelling opts out until the next capture.
  const micLive = mic === 'ready' && session !== null
  useEffect(() => {
    if (!micLive || landed || capture.kind !== 'idle') return
    startRecording()
    // startRecording is render-local and guarded by the checks above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [micLive, landed, capture.kind])

  function startRecording() {
    if (!session) return
    try {
      session.start()
      setRecorded(0)
      setLanded(false)
      setCapture({ kind: 'recording' })
    } catch (err) {
      setCapture({ kind: 'failed', message: messageOf(err) })
    }
  }

  // Cancel throws the clip away and lands in the composer, which is the other
  // way to say something.
  async function cancelRecording() {
    try {
      await session?.stop()
    } catch {
      // A cancelled clip is discarded whether or not the encoder liked it.
    }
    setLanded(true)
    setCapture({ kind: 'idle' })
  }

  async function sendRecording() {
    if (!session) return
    setCapture({ kind: 'sending' })
    let recording: Recording
    try {
      recording = await session.stop()
    } catch (err) {
      setCapture({ kind: 'failed', message: messageOf(err) })
      return
    }
    await upload(recording)
  }

  async function upload(captured: Recording) {
    setCapture({ kind: 'sending' })
    try {
      setCapture({ kind: 'thread', job: await sendAudio(captured) })
      // Show it in the sidebar now rather than up to a poll later.
      refreshOpen()
    } catch (err) {
      // Keep the blob: retrying should not mean speaking again.
      setCapture({ kind: 'failed', message: messageOf(err), recording: captured })
    }
  }

  // The draft only leaves the composer once the server has the Job.
  async function submitText(value: string): Promise<boolean> {
    try {
      setCapture({ kind: 'thread', job: await sendText(value) })
      // Show it in the sidebar now rather than up to a poll later.
      refreshOpen()
      return true
    } catch (err) {
      setCapture({ kind: 'failed', message: messageOf(err) })
      return false
    }
  }

  function resetCapture() {
    setLanded(false)
    setCapture({ kind: 'idle' })
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

  const failed = capture.kind === 'failed' ? capture : null

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
            {capture.kind === 'thread' ? (
              <JobThread job={capture.job} onReset={resetCapture} onSettled={refreshOpen} />
            ) : (
              <Boom
                session={session}
                recording={capture.kind === 'recording'}
                remainingMs={MAX_RECORDING_MS - recorded}
                onRecord={startRecording}
                onCancel={() => void cancelRecording()}
                onSend={() => void sendRecording()}
              />
            )}

            {capture.kind === 'sending' && (
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
                  <Button type="button" data-testid="retry-upload" onClick={() => void upload(failed.recording!)}>
                    Retry upload
                  </Button>
                )}
              </div>
            )}
          </Card>

          <TextComposer disabled={capture.kind === 'recording'} focus={landed} onSend={submitText} />

          <p data-testid="mic-note" className="text-xs text-muted-foreground">
            {micNote ?? 'Audio is WAV, 16 kHz, mono. The file is deleted after Blurt transcribes it.'}
          </p>
        </section>
      </main>
    </div>
  )
}