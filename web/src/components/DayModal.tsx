import { useEffect, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { cancelReminder, type Notification } from '@/lib/notifications'
import { messageOf } from '@/lib/utils'

interface DayModalProps {
  date: Date
  entries: Notification[]
  onClose: () => void
  // Called once a reminder is called off, so the month refetches without it.
  onCancelled: () => void
}

// Moment is one instant of the day: everything that lands at the same minute
// shares a bullet, and the entries are what is written beside it.
type Moment = { at: number; clock: string; entries: Notification[] }

function moments(entries: Notification[]): Moment[] {
  const grouped = new Map<number, Moment>()
  for (const entry of entries) {
    const at = Date.parse(entry.at)
    const moment = grouped.get(at) ?? {
      at,
      clock: new Date(entry.at).toLocaleTimeString(undefined, {
        hour: 'numeric',
        minute: '2-digit',
        timeZone: entry.timezone,
      }),
      entries: [],
    }
    moment.entries.push(entry)
    grouped.set(at, moment)
  }
  return [...grouped.values()].sort((a, b) => a.at - b.at)
}

// DayModal is one day read as a clock: a time on the left, a bullet on the axis,
// and what lands there on the right, running down to the end of the day. It is
// where a reminder is called off, since it is the close-up of the dot it drew.
export function DayModal({ date, entries, onClose, onCancelled }: DayModalProps) {
  const [busy, setBusy] = useState<number | null>(null)
  const [failure, setFailure] = useState<string | null>(null)

  useEffect(() => {
    const close = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', close)
    return () => window.removeEventListener('keydown', close)
  }, [onClose])

  async function callOff(reminderId: number) {
    setBusy(reminderId)
    try {
      await cancelReminder(reminderId)
      onCancelled()
    } catch (err) {
      setFailure(messageOf(err))
    } finally {
      setBusy(null)
    }
  }

  const timeline = moments(entries)
  const label = date.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' })

  return (
    <div
      data-testid="day-modal"
      onClick={onClose}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 px-3 py-6"
    >
      <Card
        role="dialog"
        aria-modal="true"
        aria-label={`${label}, ${timeline.length} moments`}
        onClick={(event) => event.stopPropagation()}
        className="w-full max-w-md gap-0 overflow-hidden p-0 shadow-2xl"
      >
        <div className="flex items-center justify-between gap-2 border-b border-border/60 px-4 py-3">
          <div>
            <h2 className="text-sm font-semibold">{label}</h2>
            <p className="text-xs text-muted-foreground">
              {entries.length === 1 ? '1 reminder' : `${entries.length} reminders`}
            </p>
          </div>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            data-testid="day-modal-close"
            aria-label="Close day"
            onClick={onClose}
            className="size-8 shrink-0"
          >
            <span aria-hidden="true">✕</span>
          </Button>
        </div>

        <div className="flex max-h-[60vh] flex-col gap-3 overflow-y-auto px-4 py-4">
          {failure && (
            <Alert variant="destructive" data-testid="day-error">
              <AlertDescription className="text-destructive">
                {failure}
              </AlertDescription>
            </Alert>
          )}

          {timeline.length === 0 ? (
            <p className="text-sm text-muted-foreground">Nothing on this day.</p>
          ) : (
            <ol className="flex flex-col">
              {timeline.map((moment, index) => (
                <li
                  key={moment.at}
                  className="grid grid-cols-[4.5rem_1.25rem_1fr] gap-x-3"
                >
                  <span
                    data-testid="day-time"
                    className="pt-0.5 text-right text-xs tabular-nums text-muted-foreground"
                  >
                    {moment.clock}
                  </span>
                  <span className="relative flex justify-center">
                    {/* The axis: one segment per moment, so a long day draws
                        one unbroken line down the middle. */}
                    <span
                      aria-hidden="true"
                      className={`absolute bottom-0 w-px bg-border ${index === 0 ? 'top-2' : 'top-0'}`}
                    />
                    <span
                      aria-hidden="true"
                      className="relative mt-1.5 size-2.5 rounded-full bg-emerald-400 motion-safe:animate-pulse"
                    />
                  </span>
                  <div className="flex flex-col gap-2 pb-5">
                    {moment.entries.map((entry) => (
                      <div
                        key={`${entry.reminderId}-${entry.at}`}
                        className="flex items-start justify-between gap-2"
                      >
                        <p data-testid="day-reminder" className="text-sm leading-relaxed">
                          {entry.text}
                        </p>
                        {moment.at > Date.now() && (
                          <Button
                            type="button"
                            variant="destructive"
                            size="icon"
                            data-testid="cancel-reminder"
                            aria-label={`Call off ${entry.text}`}
                            title="Call off"
                            disabled={busy === entry.reminderId}
                            onClick={() => void callOff(entry.reminderId)}
                            className="size-8 shrink-0"
                          >
                            <svg
                              viewBox="0 0 24 24"
                              className="size-4"
                              fill="none"
                              stroke="currentColor"
                              strokeWidth="2"
                              strokeLinecap="round"
                              strokeLinejoin="round"
                              aria-hidden="true"
                            >
                              <path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M10 11v6M14 11v6" />
                            </svg>
                          </Button>
                        )}
                      </div>
                    ))}
                  </div>
                </li>
              ))}
              <li className="grid grid-cols-[4.5rem_1.25rem_1fr] gap-x-3">
                <span className="pt-0.5 text-right text-xs tabular-nums text-muted-foreground">
                  11:59 PM
                </span>
                <span className="relative flex justify-center">
                  <span
                    aria-hidden="true"
                    className="absolute top-0 h-1.5 w-px bg-border"
                  />
                  <span
                    aria-hidden="true"
                    className="relative mt-1 size-2 rounded-full border border-border"
                  />
                </span>
                <span className="text-xs text-muted-foreground">End of day</span>
              </li>
            </ol>
          )}
        </div>
      </Card>
    </div>
  )
}
