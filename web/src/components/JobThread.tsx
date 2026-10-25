import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { PullUpMic } from '@/components/PullUpMic'
import { TextComposer } from '@/components/TextComposer'
import { Button } from '@/components/ui/button'
import { getJob, isSettled, replyToJob, type Job } from '@/lib/jobs'
import { cn } from '@/lib/utils'

interface JobThreadProps {
  job: Job
  onReset: () => void
  // Called once the thread stops changing, so a list that does not poll can
  // refresh on that event instead of on a timer.
  onSettled?: () => void
}

// JobThread shows a capture's conversation and polls until the transcript has
// landed, the user has been asked something, or the Job has failed. It owns the
// polling because it is only mounted while a thread is on screen.
export function JobThread({ job: created, onReset, onSettled }: JobThreadProps) {
  const navigate = useNavigate()
  const [job, setJob] = useState(created)
  const [replying, setReplying] = useState(false)
  const settled = isSettled(job)

  // The server hands the Job back queued, which clears `settled` and starts the
  // poll again: the same loop the capture that opened this thread ran on.
  async function reply(text: string) {
    setReplying(true)
    try {
      setJob(await replyToJob(job.jobId, text))
      return true
    } catch (err: unknown) {
      console.warn('Blurt: reply failed', err)
      return false
    } finally {
      setReplying(false)
    }
  }

  // The event a non-polling list waits for. It also fires for a thread that was
  // already settled when it mounted, which is harmless: the caller's own
  // refresh is idempotent.
  useEffect(() => {
    if (settled) onSettled?.()
  }, [settled, onSettled])

  useEffect(() => {
    if (settled) return
    let live = true
    const timer = window.setInterval(() => {
      getJob(job.jobId)
        .then((next) => {
          if (live) setJob(next)
        })
        .catch((err: unknown) => console.warn('Blurt: job poll failed', err))
    }, 1000)
    return () => {
      live = false
      window.clearInterval(timer)
    }
  }, [job.jobId, settled])

  return (
    <div data-testid="job-thread" aria-live="polite" className="flex flex-col gap-3">
      {job.conversation.map((message, index) => (
        <p
          key={`${message.at}-${index}`}
          data-testid="job-message"
          className={cn(
            'max-w-[85%] rounded-lg px-3 py-2 text-sm',
            message.role === 'user'
              ? 'self-end bg-secondary text-secondary-foreground'
              : 'self-start bg-muted text-muted-foreground',
          )}
        >
          {message.text || (message.audio ? (job.status === 'failed' ? 'Voice clip' : 'Working…') : '')}
        </p>
      ))}
      {!settled && (
        <p data-testid="capture-status" className="text-xs text-muted-foreground">
          Blurt is working…
        </p>
      )}
      {job.status === 'clarifying' && (
        <div className="flex flex-col gap-4">
          {/* The voice cue sits above the composer, where there is room to drag
              it up, and the composer stays for anyone who would rather type. */}
          <PullUpMic onConfirm={() => navigate(`/jobs/${job.jobId}/reply`)} />
          <TextComposer
            disabled={replying}
            focus
            onSend={reply}
            textTestId="reply-text"
            sendTestId="send-reply"
            placeholder="Answer Blurt…"
          />
        </div>
      )}
      <Button type="button" variant="ghost" size="sm" className="self-start" onClick={onReset}>
        New capture
      </Button>
    </div>
  )
}