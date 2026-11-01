import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { Skeleton } from '@/components/ui/skeleton'
import { Textarea } from '@/components/ui/textarea'
import { ApiError } from '@/lib/api'
import {
  failureModes,
  getNotificationDetail,
  submitNotificationFeedback,
  type FailureMode,
  type Notification,
} from '@/lib/notifications'
import { messageOf } from '@/lib/utils'

// whenIn reads one instant in the zone the Reminder was scheduled in, so a reminder
// set for 8pm in Tokyo reads as 8pm wherever the notification is opened.
function whenIn(instant: string, timezone: string): string {
  return new Intl.DateTimeFormat(undefined, {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    timeZoneName: 'short',
    timeZone: timezone,
  }).format(new Date(instant))
}

type Phase = 'loading' | 'ready' | 'done' | 'gone' | 'error'

// The page a push opens. A notification is the Reminder itself, so the id is the
// reminder's; its words come from the Reminder, never a Trace row. Feedback is
// offered behind a disclosure because most notifications need no report, and the
// page always has a way out.
export default function NotificationDetail() {
  const { reminderId } = useParams()
  const navigate = useNavigate()
  const id = Number(reminderId)
  const invalid = !Number.isInteger(id) || id < 1
  const [detail, setDetail] = useState<Notification | null>(null)
  const [phase, setPhase] = useState<Phase>(invalid ? 'gone' : 'loading')
  const [open, setOpen] = useState(false)
  const [mode, setMode] = useState<FailureMode | ''>('')
  const [comment, setComment] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    if (invalid) return
    getNotificationDetail(id)
      .then((next) => {
        if (!live) return
        setDetail(next)
        setPhase('ready')
      })
      .catch((err: unknown) => {
        if (!live) return
        setFailure(messageOf(err))
        setPhase(err instanceof ApiError && err.status === 404 ? 'gone' : 'error')
      })
    return () => {
      live = false
    }
  }, [id, invalid])

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!mode || !detail) return
    setSubmitting(true)
    setFailure(null)
    try {
      await submitNotificationFeedback(id, mode, comment)
      setPhase('done')
      setOpen(false)
    } catch (err) {
      // 404 means the notification is gone, so the form goes with it.
      if (err instanceof ApiError && err.status === 404) {
        setPhase('gone')
      } else {
        setFailure(messageOf(err))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <main className="mx-auto flex min-h-dvh w-full max-w-xl flex-col gap-5 px-4 py-6 md:py-8">
      <header className="flex items-center justify-between gap-3">
        <div>
          <p className="text-xs uppercase tracking-[0.16em] text-primary">Reminder</p>
          <h1 className="text-2xl font-semibold tracking-tight md:text-3xl">Your notification</h1>
        </div>
        <Button variant="ghost" size="sm" data-testid="notification-close" onClick={() => navigate('/home')}>
          Close
        </Button>
      </header>

      {phase === 'loading' && (
        <Card data-testid="notification-loading" className="gap-3 bg-card/80 p-5">
          <Skeleton className="h-5 w-2/3" />
          <Skeleton className="h-4 w-1/2" />
        </Card>
      )}

      {phase === 'error' && (
        <Alert variant="destructive" data-testid="notification-error">
          <AlertDescription className="text-destructive">{failure}</AlertDescription>
        </Alert>
      )}

      {phase === 'gone' && (
        <Card data-testid="notification-gone" className="gap-2 bg-card/80 p-5">
          <p className="font-medium">This notification is no longer available.</p>
          <p className="text-sm text-muted-foreground">
            It may have been removed, or it was never yours to open.
          </p>
        </Card>
      )}

      {detail && phase !== 'gone' && (
        <Card className="gap-5 bg-card/80 p-5">
          <div className="flex flex-col gap-1.5">
            <p data-testid="notification-text" className="text-xl font-semibold tracking-tight md:text-2xl">
              {detail.text}
            </p>
            <p data-testid="notification-when" className="text-sm text-muted-foreground">
              Scheduled {whenIn(detail.at, detail.timezone)}
            </p>
          </div>

          {phase === 'done' && (
            <div data-testid="feedback-done" className="flex flex-col gap-1 border-t border-border/60 pt-4">
              <p className="font-medium">Thanks — noted.</p>
              <p className="text-sm text-muted-foreground">
                We use this to make the next reminder better.
              </p>
            </div>
          )}

          {phase === 'ready' && (
            <Collapsible open={open} onOpenChange={setOpen} className="border-t border-border/60 pt-3">
              <CollapsibleTrigger asChild>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  data-testid="feedback-toggle"
                  className="-ml-2 w-fit text-muted-foreground"
                >
                  {open ? 'Hide feedback' : 'Something wrong with this reminder?'}
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent className="pt-3">
                <form onSubmit={submit} className="flex flex-col gap-4">
                  <RadioGroup
                    aria-label="What went wrong?"
                    value={mode}
                    onValueChange={(value) => setMode(value as FailureMode)}
                    className="gap-1"
                  >
                    {failureModes.map((option) => (
                      <div key={option.value} className="flex items-center gap-3 rounded-md px-1 py-2">
                        <RadioGroupItem
                          value={option.value}
                          id={`feedback-${option.value}`}
                          data-testid={`feedback-${option.value}`}
                        />
                        <Label htmlFor={`feedback-${option.value}`} className="cursor-pointer text-foreground">
                          {option.label}
                        </Label>
                      </div>
                    ))}
                  </RadioGroup>

                  <div className="flex flex-col gap-2">
                    <Label htmlFor="feedback-comment" className="text-muted-foreground">
                      Anything else? (optional)
                    </Label>
                    <Textarea
                      id="feedback-comment"
                      data-testid="feedback-comment"
                      value={comment}
                      maxLength={1000}
                      onChange={(event) => setComment(event.target.value)}
                      placeholder="A detail helps us fix it."
                    />
                  </div>

                  {failure && (
                    <Alert variant="destructive" data-testid="feedback-error">
                      <AlertDescription className="text-destructive">{failure}</AlertDescription>
                    </Alert>
                  )}

                  <div className="flex justify-end gap-2">
                    <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
                      Cancel
                    </Button>
                    <Button type="submit" data-testid="feedback-submit" disabled={!mode || submitting}>
                      {submitting ? 'Sending…' : 'Send'}
                    </Button>
                  </div>
                </form>
              </CollapsibleContent>
            </Collapsible>
          )}
        </Card>
      )}

      {phase !== 'loading' && (
        <Button variant="outline" className="w-full" onClick={() => navigate('/calendar')}>
          Back to calendar
        </Button>
      )}
    </main>
  )
}
