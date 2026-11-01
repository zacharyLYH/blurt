import { useEffect, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { getJob, isSettled, type Job } from '@/lib/jobs'
import { cn } from '@/lib/utils'

interface JobThreadProps {
  job: Job
  onReset?: () => void
  // Called once the thread stops changing, so a list that does not poll can
  // refresh on that event instead of on a timer.
  onSettled?: () => void
  // Called whenever the thread advances, so a parent that renders the composer
  // knows whether the Job is asking a question or still working.
  onChange?: (job: Job) => void
}

// JobThread shows a capture's conversation and polls until the transcript has
// landed, the user has been asked something, or the Job has failed. It owns the
// polling because it is only mounted while a thread is on screen.
export function JobThread({ job: created, onReset, onSettled, onChange }: JobThreadProps) {
  const [polled, setPolled] = useState<Job | null>(null)
  // The parent owns a reply's new turn, the poll owns everything after it. The
  // longer conversation wins, so a late poll can never roll the thread back.
  const job =
    polled && polled.jobId === created.jobId && polled.conversation.length >= created.conversation.length
      ? polled
      : created
  const settled = isSettled(job)
  const end = useRef<HTMLDivElement | null>(null)

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
          if (!live) return
          setPolled(next)
          onChange?.(next)
        })
        .catch((err: unknown) => console.warn('Blurt: job poll failed', err))
    }, 1000)
    return () => {
      live = false
      window.clearInterval(timer)
    }
  }, [job.jobId, settled, onChange])

  // A new turn settles under the message that asked for it, the way a chat does.
  useEffect(() => {
    end.current?.scrollIntoView({ block: 'end' })
  }, [job.conversation.length, job.status])

  return (
    <div data-testid="job-thread" aria-live="polite" className="flex flex-col gap-5">
      {job.conversation.map((message, index) => (
        <div
          key={`${message.at}-${index}`}
          className={cn('flex w-full', message.role === 'user' ? 'justify-end' : 'justify-start')}
        >
          <p
            data-testid="job-message"
            className={cn(
              'whitespace-pre-wrap text-sm leading-relaxed',
              message.role === 'user'
                ? 'max-w-[85%] rounded-2xl bg-secondary px-4 py-2.5 text-secondary-foreground'
                : 'max-w-[85%] text-foreground',
            )}
          >
            {message.text || (message.audio ? (job.status === 'failed' ? 'Voice clip' : 'Working…') : '')}
          </p>
        </div>
      ))}
      {!settled && (
        <p data-testid="capture-status" className="text-xs text-muted-foreground">
          Blurt is working…
        </p>
      )}
      <div ref={end} />
      {onReset && (
        <Button type="button" variant="ghost" size="sm" className="self-start text-muted-foreground" onClick={onReset}>
          New chat
        </Button>
      )}
    </div>
  )
}
