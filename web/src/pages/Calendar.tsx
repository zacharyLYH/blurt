import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { Calendar as MonthCalendar } from '@/components/ui/calendar'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Item, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { Skeleton } from '@/components/ui/skeleton'
import {
  listNotificationsForMonth,
  type Notification,
  type NotificationMonth,
} from '@/lib/notifications'
import { cn, messageOf } from '@/lib/utils'

const pad = (value: number) => String(value).padStart(2, '0')
const monthKey = (date: Date) => `${date.getFullYear()}-${pad(date.getMonth() + 1)}`
const dateKey = (date: Date) => `${monthKey(date)}-${pad(date.getDate())}`

// Dates are the device's own calendar days: the month in the URL is what the user
// calls "this month", not the server's UTC month.
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

function dateFromKey(key: string): Date {
  const [year, month, day] = key.split('-').map(Number)
  return new Date(year, month - 1, day)
}

// dayKey is the calendar day an instant belongs to in the Reminder's own zone, so a
// reminder set for 11pm Singapore time does not slide onto the previous day when the
// server stores it in UTC.
function dayKey(instant: string, timezone: string): string {
  return new Intl.DateTimeFormat('en-CA', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    timeZone: timezone,
  }).format(new Date(instant))
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
    <button {...props} data-testid={`calendar-day-${dateKey(day.date)}`}>
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

// The calendar is Reminder-only and scoped to the month in the URL. A month change
// refetches; nothing is cached. The grid shows where things are, and the list below
// shows what the selected day holds, which stays readable on a phone.
export default function Calendar() {
  const navigate = useNavigate()
  const [params, setParams] = useSearchParams()
  const month = params.get('month') ?? monthKey(new Date())
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
  const [data, setData] = useState<NotificationMonth | null>(null)
  const [failure, setFailure] = useState<string | null>(null)
  const [chosen, setChosen] = useState<Date | null>(null)
  // Loading is derived: a month the server has not answered for yet is loading.
  const loading = data?.month !== month

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
  }, [month, timezone])

  const days = useMemo(() => {
    const grouped = new Map<string, Notification[]>()
    for (const entry of data?.entries ?? []) {
      const key = dayKey(entry.at, entry.timezone)
      grouped.set(key, [...(grouped.get(key) ?? []), entry])
    }
    return grouped
  }, [data])
  const reminderDays = useMemo(() => [...days.keys()].map(dateFromKey), [days])

  // Selection follows the month unless the user picked a day in it. On a new month the
  // first reminder day is the useful default, so the list below is never empty by
  // accident.
  const fallback = reminderDays[0] ?? (monthKey(new Date()) === month ? new Date() : parseMonth(month))
  const selected = chosen && monthKey(chosen) === month ? chosen : fallback
  const selectedEntries = days.get(dateKey(selected)) ?? []

  function go(next: string) {
    setChosen(null)
    setParams({ month: next })
  }

  const previous = shiftMonth(month, -1)
  const next = shiftMonth(month, 1)
  const dayLabel = selected.toLocaleDateString(undefined, {
    weekday: 'long',
    month: 'long',
    day: 'numeric',
  })

  return (
    <main className="mx-auto flex min-h-dvh w-full max-w-2xl flex-col gap-4 px-3 py-6 md:px-4 md:py-8">
      <header className="flex items-start justify-between gap-3 px-1">
        <div>
          <p className="text-xs uppercase tracking-[0.16em] text-primary">Calendar</p>
          <h1 data-testid="calendar-month" className="text-2xl font-semibold tracking-tight md:text-3xl">
            {monthLabel(month)}
          </h1>
        </div>
        <Button variant="ghost" size="sm" onClick={() => navigate('/home')}>
          Back
        </Button>
      </header>

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
              onSelect={(day) => day && setChosen(day)}
              hideNavigation
              modifiers={{ hasReminder: reminderDays }}
              components={{ DayButton: ReminderDay }}
            />

            <section data-testid="calendar-day-list" className="mt-4 flex flex-col gap-3 border-t border-border/60 pt-4">
              <div className="flex items-baseline justify-between gap-3">
                <h2 className="text-sm font-medium">{dayLabel}</h2>
                <span className="text-xs text-muted-foreground">
                  {selectedEntries.length === 1 ? '1 reminder' : `${selectedEntries.length} reminders`}
                </span>
              </div>
              {selectedEntries.length === 0 ? (
                <p data-testid="calendar-empty" className="text-sm text-muted-foreground">
                  Nothing on this day.
                </p>
              ) : (
                <ItemGroup className="gap-2">
                  {selectedEntries.map((entry) => (
                    <Item key={`${entry.reminderId}-${entry.at}`} asChild variant="outline" size="sm">
                      <Link to={`/notifications/${entry.reminderId}`} data-testid="calendar-entry">
                        <ItemContent>
                          <ItemTitle className="w-full truncate">{entry.text}</ItemTitle>
                          <ItemDescription>{timeIn(entry.at, entry.timezone)}</ItemDescription>
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
    </main>
  )
}
