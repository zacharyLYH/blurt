import { useCallback, useEffect, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { AppShell } from '@/components/AppShell'
import { DayModal } from '@/components/DayModal'
import { Halo } from '@/components/Halo'
import { TextComposer } from '@/components/TextComposer'
import { VoiceScreen } from '@/components/VoiceScreen'
import { Calendar as MonthCalendar } from '@/components/ui/calendar'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Item, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { Skeleton } from '@/components/ui/skeleton'
import questionnaire from '@/data/questionnaire/v1.json'
import { api } from '@/lib/api'
import { listJobs, sendAudio, sendText, type JobSummary } from '@/lib/jobs'
import { listNotificationsForMonth, type NotificationMonth } from '@/lib/notifications'
import { setupPush, type PushSetup } from '@/lib/push'
import { useMicrophone, type MicStatus } from '@/lib/useMicrophone'
import { MAX_RECORDING_MS, useRecorder } from '@/lib/useRecorder'
import { useUser } from '@/lib/user'
import { cn, messageOf } from '@/lib/utils'

const micCopy: Record<MicStatus, { label: string; dot: string }> = {
  starting: { label: 'Checking mic…', dot: 'bg-primary' },
  ready: { label: 'Mic ready', dot: 'bg-emerald-400' },
  denied: { label: 'Mic blocked', dot: 'bg-destructive' },
  unavailable: { label: 'No mic', dot: 'bg-muted-foreground' },
  unsupported: { label: 'Voice unsupported', dot: 'bg-muted-foreground' },
}

// Dates are the device's own calendar days: the month in the URL is what the user
// calls "this month", not the server's UTC month. en-CA is the locale whose date
// is YYYY-MM-DD, so a day compares as one string, and a day out of an instant is
// read in the Reminder's own zone — which keeps an 11pm reminder on its own day.
const dayStamp = (date: Date, timezone?: string) => date.toLocaleDateString('en-CA', { timeZone: timezone })
const monthKey = (date: Date) => dayStamp(date).slice(0, 7)
const dateFromKey = (key: string) => new Date(`${key}T12:00:00`)

function parseMonth(month: string): Date {
  const [year, index] = month.split('-').map(Number)
  return new Date(year, index - 1, 1)
}

function shiftMonth(month: string, by: number): string {
  const start = parseMonth(month)
  return monthKey(new Date(start.getFullYear(), start.getMonth() + by, 1))
}

function monthLabel(month: string): string {
  return parseMonth(month).toLocaleDateString(undefined, { month: 'long', year: 'numeric' })
}

function timeIn(instant: string, timezone: string): string {
  return new Intl.DateTimeFormat(undefined, {
    hour: 'numeric',
    minute: '2-digit',
    timeZoneName: 'short',
    timeZone: timezone,
  }).format(new Date(instant))
}

// ReminderDay is the only thing the page adds to a day cell: the number and a dot
// when something lands there. The grid itself, leap days and keyboard movement are
// react-day-picker's.
function ReminderDay({
  day,
  modifiers,
  ...props
}: {
  day: { date: Date }
  modifiers: Record<string, boolean>
} & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button {...props} data-testid={`calendar-day-${dayStamp(day.date)}`}>
      <span className="text-xs font-medium md:text-sm">{day.date.getDate()}</span>
      <span
        aria-hidden="true"
        className={cn(
          'mt-1 size-1.5 rounded-full',
          modifiers.hasReminder ? 'bg-primary' : 'bg-transparent',
        )}
      />
    </button>
  )
}

// Home is the calendar: what is already scheduled, the halo that opens the boom,
// and the composer for the times a blurt is easier typed. A capture never opens a
// thread — if Blurt needs an answer it lands in the inbox, and only an inbox row
// opens a conversation.
export default function Calendar() {
  const navigate = useNavigate()
  const { user, setUser } = useUser()
  const [params, setParams] = useSearchParams()
  const month = params.get('month') ?? monthKey(new Date())
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
  const [data, setData] = useState<NotificationMonth | null>(null)
  const [failure, setFailure] = useState<string | null>(null)
  const [chosen, setChosen] = useState<Date | null>(null)
  // The day whose timeline is open. Tapping a date is the only way in, so the
  // month stays the overview and the day gets the close-up.
  const [dayOpen, setDayOpen] = useState<Date | null>(null)
  const [inbox, setInbox] = useState<JobSummary[]>([])
  const [push, setPush] = useState<PushSetup>('pending')
  // Bumped after a capture, so the month refetches and a new dot appears without
  // the user reloading to look for proof.
  const [captures, setCaptures] = useState(0)
  // Loading is derived: a month the server has not answered for yet is loading.
  const loading = data?.month !== month

  const { status: mic, note: micNote, session, retry: retryMic } = useMicrophone()

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

  // The inbox is questions only: an open Job is still being worked on, and one
  // that closed has nothing to answer.
  const questions = inbox.filter((job) => job.status === 'clarifying')
  const refreshInbox = useCallback(() => {
    void listJobs()
      .then(setInbox)
      .catch((err: unknown) => console.warn('Blurt: inbox failed', err))
  }, [])

  useEffect(() => {
    refreshInbox()
  }, [refreshInbox])

  // A capture lands as a working Job and only becomes a question a beat later,
  // so while anything is still in flight the inbox is the one place that would
  // notice. Poll until nothing is left in flight, then stop.
  const working = inbox.some((job) => job.status === 'open')
  useEffect(() => {
    if (!working) return
    const timer = window.setInterval(refreshInbox, 2000)
    return () => window.clearInterval(timer)
  }, [working, refreshInbox])

  // A capture may want an answer a beat later, so the inbox and the month both
  // re-ask once the server has it.
  function afterCapture() {
    refreshInbox()
    setCaptures((count) => count + 1)
  }

  // The boom opens by itself on entry: a voice-first app should be one word from
  // recording. Cancelling opts out until the halo asks again.
  const recorder = useRecorder(
    session,
    async (recording) => {
      await sendAudio(recording)
      afterCapture()
    },
    true,
  )

  // The draft only leaves the composer once the server has the blurt.
  async function submitText(value: string): Promise<boolean> {
    try {
      await sendText(value)
      afterCapture()
      return true
    } catch (err) {
      recorder.fail(messageOf(err))
      return false
    }
  }

  async function signOut() {
    try {
      await api<void>('/api/auth/logout', { method: 'POST' })
    } catch (err) {
      // The local session is gone either way: a failed revoke must not trap the
      // user on Home.
      console.warn('Blurt: logout request failed', err)
    } finally {
      setUser(null)
      navigate('/login', { replace: true })
    }
  }

  useEffect(() => {
    let live = true
    listNotificationsForMonth(month, timezone)
      .then((next) => {
        if (!live) return
        setData(next)
        setFailure(null)
      })
      .catch((err: unknown) => {
        if (live) setFailure(messageOf(err))
      })
    return () => {
      live = false
    }
  }, [month, timezone, captures])

  const entries = data?.entries ?? []
  const reminderDays = [
    ...new Set(entries.map((entry) => dayStamp(new Date(entry.at), entry.timezone))),
  ].map(dateFromKey)
  const entriesOn = (day: Date) =>
    entries.filter(
      (entry) => dayStamp(new Date(entry.at), entry.timezone) === dayStamp(day),
    )

  // Selection follows the month unless the user picked a day in it. On a new month the
  // first reminder day is the useful default, so the list below is never empty by
  // accident.
  const fallback = reminderDays[0] ?? (monthKey(new Date()) === month ? new Date() : parseMonth(month))
  const selected = chosen && monthKey(chosen) === month ? chosen : fallback
  const selectedEntries = entriesOn(selected)

  function go(next: string) {
    setChosen(null)
    setDayOpen(null)
    setParams({ month: next })
  }

  const previous = shiftMonth(month, -1)
  const next = shiftMonth(month, 1)
  const dayLabel = selected.toLocaleDateString(undefined, {
    weekday: 'long',
    month: 'long',
    day: 'numeric',
  })

  const failed = recorder.state.kind === 'failed' ? recorder.state : null
  const recording = recorder.state.kind === 'recording'
  const sending = recorder.state.kind === 'sending'
  // Every other mic state needs the retry beside the composer, because typing
  // is then the whole voice path.
  const micBroken = mic === 'denied' || mic === 'unavailable' || mic === 'unsupported'

  return (
    <>
      <AppShell
        inbox={questions}
        push={push}
        onSignOut={() => void signOut()}
        onOpenInbox={refreshInbox}
      >
        <main className="flex min-h-0 flex-1 flex-col">
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="mx-auto flex w-full max-w-2xl flex-col gap-4 px-3 py-6 md:px-4 md:py-8">
              <div className="px-1">
                <p className="text-xs uppercase tracking-[0.16em] text-primary">Calendar</p>
                <h1 data-testid="calendar-month" className="text-2xl font-semibold tracking-tight md:text-3xl">
                  {monthLabel(month)}
                </h1>
              </div>

              {failure && (
                <Alert variant="destructive" data-testid="calendar-error">
                  <AlertDescription className="text-destructive">{failure}</AlertDescription>
                </Alert>
              )}

              <Card className="gap-0 bg-card/60 p-3 md:p-5">
                <div className="flex items-center justify-between gap-2 pb-2">
                  <Button
                    variant="ghost"
                    size="sm"
                    data-testid="calendar-prev"
                    aria-label={`Previous month, ${monthLabel(previous)}`}
                    onClick={() => go(previous)}
                  >
                    <span aria-hidden="true">‹</span> Previous
                  </Button>
                  <Button variant="ghost" size="sm" onClick={() => go(monthKey(new Date()))}>
                    Today
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    data-testid="calendar-next"
                    aria-label={`Next month, ${monthLabel(next)}`}
                    onClick={() => go(next)}
                  >
                    Next <span aria-hidden="true">›</span>
                  </Button>
                </div>

                {loading ? (
                  <div data-testid="calendar-loading" className="flex flex-col gap-2 py-3">
                    <Skeleton className="mx-auto h-4 w-28" />
                    <div className="grid grid-cols-7 gap-1">
                      {Array.from({ length: 35 }, (_, index) => (
                        <Skeleton key={index} className="h-10 rounded-md md:h-12" />
                      ))}
                    </div>
                  </div>
                ) : (
                  <>
                    <MonthCalendar
                      mode="single"
                      month={parseMonth(month)}
                      selected={selected}
                      onSelect={(day) => {
                        // Re-clicking the selected day must not clear it: closing
                        // the modal should leave the list where it was.
                        if (day) setChosen(day)
                      }}
                      onDayClick={(day) => {
                        // A tapped day is the user's choice, even when it was
                        // already the default: it must not drift when a
                        // reminder elsewhere is called off.
                        setChosen(day)
                        setDayOpen(day)
                      }}
                      hideNavigation
                      modifiers={{ hasReminder: reminderDays }}
                      components={{ DayButton: ReminderDay }}
                    />

                    <section
                      data-testid="calendar-day-list"
                      className="mt-4 flex flex-col gap-3 border-t border-border/60 pt-4"
                    >
                      <div className="flex items-baseline justify-between gap-3">
                        <h2 className="text-sm font-medium">{dayLabel}</h2>
                        <span className="text-xs text-muted-foreground">
                          {selectedEntries.length === 1
                            ? '1 reminder'
                            : `${selectedEntries.length} reminders`}
                        </span>
                      </div>
                      {selectedEntries.length === 0 ? (
                        <p
                          data-testid="calendar-empty"
                          className="text-sm text-muted-foreground"
                        >
                          Nothing on this day.
                        </p>
                      ) : (
                        <ItemGroup className="gap-2">
                          {selectedEntries.map((entry) => (
                            <Item
                              key={`${entry.reminderId}-${entry.at}`}
                              asChild
                              variant="outline"
                              size="sm"
                            >
                              <Link
                                to={`/notifications/${entry.reminderId}`}
                                data-testid="calendar-entry"
                              >
                                <ItemContent>
                                  <ItemTitle className="w-full truncate">
                                    {entry.text}
                                  </ItemTitle>
                                  <ItemDescription>
                                    {timeIn(entry.at, entry.timezone)}
                                  </ItemDescription>
                                </ItemContent>
                                <span aria-hidden="true" className="text-muted-foreground">
                                  ›
                                </span>
                              </Link>
                            </Item>
                          ))}
                        </ItemGroup>
                      )}
                    </section>
                  </>
                )}
              </Card>

              <p className="px-1 text-xs text-muted-foreground">
                Tap a day to see its reminders. Times read in each reminder&apos;s own zone.
              </p>
            </div>
          </div>

          <div className="shrink-0 border-t border-border/60 bg-background/85 px-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] pt-3">
            <div className="mx-auto flex w-full max-w-2xl flex-col gap-1">
              {failed && (
                <div className="flex items-center justify-between gap-2 pb-1">
                  <Alert variant="destructive" data-testid="capture-error">
                    <AlertDescription className="text-destructive">{failed.message}</AlertDescription>
                  </Alert>
                  {failed.recording && (
                    <Button
                      type="button"
                      data-testid="retry-upload"
                      onClick={() => void recorder.retry()}
                    >
                      Retry
                    </Button>
                  )}
                </div>
              )}
              <TextComposer
                disabled={sending}
                focus={recorder.landed}
                onSend={submitText}
              />
              <div className="flex items-center justify-between gap-2 px-2 text-xs text-muted-foreground">
                <span
                  data-testid="mic-status"
                  className="flex min-w-0 items-center gap-1.5"
                >
                  <span
                    className={cn('size-2 shrink-0 rounded-full', micCopy[mic].dot)}
                    aria-hidden="true"
                  />
                  <span className="truncate">{micCopy[mic].label}</span>
                </span>
                {micBroken && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={retryMic}
                  >
                    Try mic again
                  </Button>
                )}
              </div>
              {micNote && (
                <p data-testid="mic-note" className="truncate px-2 text-xs text-muted-foreground">
                  {micNote}
                </p>
              )}
            </div>
          </div>
        </main>
      </AppShell>

      <Halo onSpawn={recorder.start} disabled={!session || sending} />

      {dayOpen && (
        <DayModal
          date={dayOpen}
          entries={entriesOn(dayOpen)}
          onClose={() => setDayOpen(null)}
          onCancelled={() => setCaptures((count) => count + 1)}
        />
      )}

      {recording && (
        <VoiceScreen
          session={session}
          remainingMs={MAX_RECORDING_MS - recorder.recorded}
          onRecord={recorder.start}
          onCancel={() => void recorder.cancel()}
          onSend={() => void recorder.send()}
        />
      )}

      {sending && (
        <div className="fixed inset-0 z-50 flex flex-col items-center justify-center gap-3 bg-background px-6">
          <p data-testid="capture-status" className="text-sm text-muted-foreground">
            Sending…
          </p>
        </div>
      )}
    </>
  )
}
