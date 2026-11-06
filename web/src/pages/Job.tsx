import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { JobThread } from '@/components/JobThread'
import { TextComposer } from '@/components/TextComposer'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ApiError } from '@/lib/api'
import { getJob, replyToJob, type Job as JobRecord } from '@/lib/jobs'
import { useMicrophone } from '@/lib/useMicrophone'
import { messageOf } from '@/lib/utils'

// Job is one question, opened from an inbox row. A capture never lands here on
// its own, and the way out is the first thing on the page: this is a detour from
// the calendar, not a place to live.
export default function Job() {
  const navigate = useNavigate()
  const { jobId } = useParams()
  const id = Number(jobId)
  const valid = Number.isInteger(id) && id > 0
  const [job, setJob] = useState<JobRecord | null>(null)
  const [failure, setFailure] = useState<string | null>(null)
  const { status: mic, session, retry: retryMic } = useMicrophone()

  useEffect(() => {
    if (!valid) return
    let live = true
    getJob(id)
      .then((next) => {
        if (live) setJob(next)
      })
      .catch((err: unknown) => {
        if (!live) return
        setFailure(err instanceof ApiError && err.status === 404 ? 'That capture no longer exists.' : 'Could not load that capture.')
      })
    return () => {
      live = false
    }
  }, [id, valid])

  // A malformed id never reaches the network, so its message is derived rather
  // than state.
  const error = valid ? failure : 'That capture no longer exists.'
  const asking = job?.status === 'clarifying'

  // The draft only leaves the composer once the server has the turn.
  async function reply(value: string): Promise<boolean> {
    if (!job) return false
    try {
      setJob(await replyToJob(job.jobId, value))
      return true
    } catch (err) {
      setFailure(messageOf(err))
      return false
    }
  }

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex shrink-0 items-center gap-2 border-b border-border/60 bg-black/20 px-3 pb-2 pt-[max(0.75rem,env(safe-area-inset-top))]">
        <Button
          type="button"
          variant="outline"
          size="sm"
          data-testid="exit-chat"
          onClick={() => navigate('/home')}
          className="gap-1.5"
        >
          <svg
            viewBox="0 0 24 24"
            className="size-4"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.2"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="M19 12H5M12 5l-7 7 7 7" />
          </svg>
          Calendar
        </Button>
        {asking && (
          <span className="ml-auto text-xs uppercase tracking-[0.12em] text-muted-foreground">
            Question
          </span>
        )}
      </header>

      <main className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-3xl flex-col gap-4 px-4 py-6">
          {error && (
            <Alert variant="destructive" data-testid="job-error">
              <AlertDescription className="text-destructive">{error}</AlertDescription>
            </Alert>
          )}
          {job ? (
            <JobThread job={job} onChange={setJob} />
          ) : (
            !error && (
              <div className="flex flex-col gap-3">
                <Skeleton className="h-4 w-2/3" />
                <Skeleton className="h-4 w-1/2" />
              </div>
            )
          )}
        </div>
      </main>

      {asking && (
        <div className="shrink-0 border-t border-border/60 bg-background/85 px-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] pt-3">
          <div className="mx-auto flex w-full max-w-3xl flex-col gap-1">
            <TextComposer
              onSend={reply}
              onVoice={() => navigate(`/jobs/${id}/reply`)}
              voiceDisabled={!session}
              textTestId="reply-text"
              sendTestId="send-reply"
              placeholder="Answer Blurt…"
            />
            {mic !== 'ready' && (
              <div className="flex items-center justify-between gap-2 px-2 text-xs text-muted-foreground">
                <span data-testid="mic-status">
                  Voice is unavailable — type instead.
                </span>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={retryMic}
                >
                  Try mic again
                </Button>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
