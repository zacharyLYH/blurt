import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import { getJob, isSettled, type Job } from '@/lib/jobs'
import { cn } from '@/lib/utils'

interface JobThreadProps {
  job: Job
  onReset: () => void
  // Called once the thread stops changing, so a list that does not poll can
  // refresh on that event instead of on a timer.
  onSettled?: () => void
}

// JobThread shows a capture's conversation and polls until the transcript has
// landed or the Job has failed. It owns the polling because it is only mounted
// while a thread is on screen.
export function JobThread({ job: created, onReset, onSettled }: JobThreadProps) {
  const [job, setJob] = useState(created)
  const settled = isSettled(job)

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
          {message.text || (message.audio ? (job.status === 'failed' ? 'Voice clip' : 'Transcribing…') : '')}
        </p>
      ))}
      {!settled && (
        <p data-testid="capture-status" className="text-xs text-muted-foreground">
          Blurt is transcribing…
        </p>
      )}
      <Button type="button" variant="ghost" size="sm" className="self-start" onClick={onReset}>
        New capture
      </Button>
    </div>
  )
}