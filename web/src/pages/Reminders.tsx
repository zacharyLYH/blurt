import { useCallback, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { Skeleton } from '@/components/ui/skeleton'
import { cancelReminder, listReminders, type Reminder, type ReminderLists, type ReminderRule, type ReminderStatus } from '@/lib/reminders'
import { messageOf } from '@/lib/utils'

const statusLabel: Record<ReminderStatus, string> = {
  resolved: 'Scheduled',
  fired: 'Sent',
  cancelled: 'Called off',
}

// Only a called-off one is worth colour: scheduled and sent are the two states the user
// is looking for.
const statusVariant: Record<ReminderStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  resolved: 'default',
  fired: 'outline',
  cancelled: 'destructive',
}

const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December']

// timeIn renders one instant in the zone the Reminder was scheduled in rather than the
// device's: a Reminder set for 8pm in Tokyo has to read as 8pm wherever it is opened.
function timeIn(instant: string | undefined, timezone: string): string {
  if (!instant) return ''
  const at = new Date(instant)
  try {
    return new Intl.DateTimeFormat(undefined, {
      month: 'short',
      day: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
      timeZoneName: 'short',
      timeZone: timezone,
    }).format(at)
  } catch {
    return at.toLocaleString()
  }
}

// cadence reads a rule back as the words the user would use for it, and leaves the clock
// to the instant shown beside it.
function cadence(rule: ReminderRule): string {
  switch (rule.frequency) {
    case 'daily':
      return 'Every day'
    case 'weekly':
      return `Every ${WEEKDAYS[rule.weekday ?? 0]}`
    case 'monthly':
      return `Every month on the ${ordinal(rule.day ?? 1)}`
    default:
      return `Every year on ${MONTHS[(rule.month ?? 1) - 1]} ${rule.day ?? 1}`
  }
}

function ordinal(day: number): string {
  if (day === 11 || day === 12 || day === 13) return `${day}th`
  switch (day % 10) {
    case 1:
      return `${day}st`
    case 2:
      return `${day}nd`
    case 3:
      return `${day}rd`
    default:
      return `${day}th`
  }
}

// ReminderList is one side of the page: a heading and its rows. Both sides read the same
// way, and only the upcoming one can be called off — a fired Reminder has nothing left
// to stop.
function ReminderList({ title, testId, empty, reminders, busy, onCancel }: {
  title: string
  testId: string
  empty: string
  reminders: Reminder[]
  busy: number | null
  onCancel: ((reminderId: number) => void) | null
}) {
  return (
    <section className="flex flex-col gap-2">
      <h2 className="text-xs uppercase tracking-[0.12em] text-muted-foreground">{title}</h2>
      {reminders.length === 0 ? (
        <p data-testid={`${testId}-empty`} className="text-sm text-muted-foreground">
          {empty}
        </p>
      ) : (
        <ItemGroup data-testid={testId} aria-label={title} className="gap-2">
          {reminders.map((reminder) => (
            <Item key={reminder.reminderId} variant="outline" data-testid="reminder-row">
              <ItemContent>
                <ItemTitle data-testid="reminder-text" className="w-full truncate">
                  {reminder.text}
                </ItemTitle>
                <ItemDescription data-testid="reminder-when">
                  {reminder.rule
                    ? `${cadence(reminder.rule)} · ${timeIn(reminder.nextEmitAtUtc, reminder.timezone)}`
                    : timeIn(reminder.emitAtUtc, reminder.timezone)}
                </ItemDescription>
              </ItemContent>
              <ItemActions>
                <Badge variant={statusVariant[reminder.status]}>{statusLabel[reminder.status]}</Badge>
                {onCancel && (
                  <Button
                    variant="ghost"
                    size="sm"
                    data-testid="cancel-reminder"
                    disabled={busy === reminder.reminderId}
                    onClick={() => onCancel(reminder.reminderId)}
                  >
                    Call off
                  </Button>
                )}
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
      )}
    </section>
  )
}

// The page a notification opens into. It is a route of its own rather than a panel on
// Home because the service worker can deep-link straight here on a cold start, so it
// loads what it needs without the capture screen around it.
export default function Reminders() {
  const navigate = useNavigate()
  const [lists, setLists] = useState<ReminderLists | null>(null)
  const [failure, setFailure] = useState<string | null>(null)
  const [busy, setBusy] = useState<number | null>(null)

  // Refetching is the truthful move after a cancel: the row has changed sides, and the
  // server is the one that decides where it lands.
  const refresh = useCallback(() => {
    listReminders()
      .then((next) => {
        setLists(next)
        setFailure(null)
      })
      .catch((err: unknown) => setFailure(messageOf(err)))
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  async function cancel(reminderId: number) {
    setBusy(reminderId)
    try {
      await cancelReminder(reminderId)
      refresh()
    } catch (err) {
      setFailure(messageOf(err))
    } finally {
      setBusy(null)
    }
  }

  return (
    <main className="mx-auto flex min-h-dvh w-full max-w-xl flex-col gap-6 px-4 py-6 md:py-8">
      <header className="flex items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight md:text-3xl">Reminders</h1>
        <Button variant="ghost" size="sm" onClick={() => navigate('/home')}>
          Back
        </Button>
      </header>

      {failure && (
        <Alert variant="destructive" data-testid="reminders-error">
          <AlertDescription className="text-destructive">{failure}</AlertDescription>
        </Alert>
      )}

      {!lists && !failure && (
        <Card className="gap-3 bg-card/80 p-5">
          <Skeleton className="h-4 w-2/3" />
          <Skeleton className="h-4 w-1/2" />
        </Card>
      )}

      {lists && (
        <>
          <ReminderList
            title="Upcoming"
            testId="reminder-upcoming"
            empty="Nothing scheduled yet."
            reminders={lists.upcoming}
            busy={busy}
            onCancel={cancel}
          />
          <ReminderList
            title="Past"
            testId="reminder-past"
            empty="Nothing has been sent or called off yet."
            reminders={lists.past}
            busy={busy}
            onCancel={null}
          />
        </>
      )}
    </main>
  )
}