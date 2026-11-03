import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { Skeleton } from '@/components/ui/skeleton'
import {
  getTraceHistory,
  listTracedReminders,
  type Trace,
  type TraceHistory,
  type TracedReminder,
} from '@/lib/traces'
import { cn, messageOf } from '@/lib/utils'

// A stage's colour, so the eye finds the firings among the planning rows.
const stageTint: Record<string, string> = {
  agent: 'bg-emerald-500/15 text-emerald-300',
  transcription: 'bg-sky-500/15 text-sky-300',
  notification: 'bg-primary/15 text-primary',
}

// StageBadge is a stage's chip. The row and the detail sheet show the same
// stage, so the colour and the type come from one place.
function StageBadge({ stage }: { stage: string }) {
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide',
        stageTint[stage] ?? 'bg-muted text-muted-foreground',
      )}
    >
      {stage}
    </span>
  )
}

// facts are the detail keys worth a line above the JSON: the ones a reader is
// looking for when a reminder came out wrong.
const facts = ['outcome', 'kind', 'intent', 'model', 'turns', 'attempt', 'lint']

function msOf(trace: Trace): number {
  const value = trace.detail?.durationMs
  return typeof value === 'number' ? value : 0
}

function clockOf(timestamp: string): string {
  const date = new Date(timestamp)
  return Number.isNaN(date.getTime())
    ? timestamp
    : date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

// shown reads one detail value as a line: an object stays JSON, because a nested
// payload is a fact of its own rather than a sentence.
function shown(value: unknown): string {
  if (value === undefined || value === null || value === '') return ''
  return typeof value === 'object' ? JSON.stringify(value) : String(value)
}

// byJob groups the flat rows the way the loop ran them: a capture, then everything it
// set off. Rows arrive oldest first, so one pass keeps that order.
function byJob(traces: Trace[]): { jobId: number; traces: Trace[] }[] {
  const groups: { jobId: number; traces: Trace[] }[] = []
  for (const trace of traces) {
    const current = groups[groups.length - 1]
    if (current && current.jobId === trace.jobId) current.traces.push(trace)
    else groups.push({ jobId: trace.jobId, traces: [trace] })
  }
  return groups
}

// DetailFacts is one stage's payload: the keys a reader looks for, then the whole object,
// so nothing is hidden behind a guess about what mattered.
function DetailFacts({ detail }: { detail: Record<string, unknown> }) {
  return (
    <div className="flex flex-col gap-1.5">
      {facts.map((key) => {
        const value = shown(detail[key])
        return value ? (
          <p key={key} className="text-[11px] text-muted-foreground">
            <span className="font-mono text-foreground/70">{key}</span> {value}
          </p>
        ) : null
      })}
      <pre className="overflow-x-auto rounded-md bg-muted/30 p-2 text-[11px] leading-relaxed text-muted-foreground">
        {JSON.stringify(detail, null, 2)}
      </pre>
    </div>
  )
}

// TraceRow is one stage, indented under its job. The bar is its share of the slowest
// stage in this history, which is the only comparison the rows can honestly make.
function TraceRow({
  trace,
  slowest,
  onOpen,
}: {
  trace: Trace
  slowest: number
  onOpen: (trace: Trace) => void
}) {
  const duration = msOf(trace)
  const width = `${Math.max(2, Math.round((duration / slowest) * 100))}%`

  return (
    <div className="flex items-center gap-2 rounded-md border border-border/40 bg-card/40 py-1.5 pl-4 pr-2">
      <StageBadge stage={trace.stage} />
      <span className="shrink-0 font-mono text-[11px] text-muted-foreground">
        {clockOf(trace.timestamp)}
      </span>
      <span className="h-1.5 min-w-0 flex-1 rounded bg-muted/60">
        <span className="block h-full rounded bg-primary/70" style={{ width }} />
      </span>
      <span className="w-14 shrink-0 text-right font-mono text-[11px] tabular-nums text-muted-foreground">
        {duration}ms
      </span>
      <Button
        variant="ghost"
        size="sm"
        className="h-6 shrink-0 px-2 text-[11px]"
        onClick={() => onOpen(trace)}
      >
        Detail
      </Button>
    </div>
  )
}

// Traces is the debug page: every reminder with history on the left, and the stage
// waterfall of the one chosen on the right. The id box is a filter and a jump, so a
// reminder id from a Trace or a bug report lands on its own history in one keystroke.
export default function Traces() {
  const navigate = useNavigate()
  const [reminders, setReminders] = useState<TracedReminder[] | null>(null)
  const [selected, setSelected] = useState<number | null>(null)
  const [history, setHistory] = useState<TraceHistory | null>(null)
  const [query, setQuery] = useState('')
  const [detail, setDetail] = useState<Trace | null>(null)
  const [failure, setFailure] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    listTracedReminders()
      .then((rows) => {
        if (!live) return
        setReminders(rows)
        setSelected((current) => current ?? rows[0]?.reminderId ?? null)
      })
      .catch((err: unknown) => {
        if (live) setFailure(messageOf(err))
      })
    return () => {
      live = false
    }
  }, [])

  useEffect(() => {
    if (selected === null) return
    let live = true
    getTraceHistory(selected)
      .then((next) => {
        if (live) {
          setHistory(next)
          setFailure(null)
        }
      })
      .catch((err: unknown) => {
        if (live) setFailure(messageOf(err))
      })
    return () => {
      live = false
    }
  }, [selected])

  // The filter reads ids and words alike: a bare number is the id a reader has.
  const needle = query.trim().toLowerCase()
  const shownRows = (reminders ?? []).filter(
    (row) => !needle || String(row.reminderId).includes(needle) || row.text.toLowerCase().includes(needle),
  )
  // A slow read must not show the previous reminder's rows, so the answer is read
  // only once it is the one on screen: no reset in the effect, no stale frame.
  const current = history && history.reminder.reminderId === selected ? history : null
  const groups = useMemo(() => byJob(current?.traces ?? []), [current])
  const slowest = Math.max(1, ...(current?.traces ?? []).map(msOf))

  return (
    <main className="flex min-h-dvh flex-col bg-background">
      <header className="flex shrink-0 items-center justify-between gap-3 border-b border-border/60 px-4 py-3">
        <div>
          <p className="text-xs uppercase tracking-[0.16em] text-primary">Traces</p>
          <h1 className="text-xl font-semibold tracking-tight md:text-2xl">What the stages did</h1>
        </div>
        <Button variant="ghost" size="sm" onClick={() => navigate('/home')}>
          Home
        </Button>
      </header>

      <div className="flex min-h-0 flex-1 flex-col md:flex-row">
        <aside className="shrink-0 border-b border-border/60 p-3 md:w-80 md:overflow-y-auto md:border-b-0 md:border-r">
          <Input
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Reminder id or words"
            aria-label="Find a reminder"
            data-testid="trace-filter"
          />

          {reminders === null ? (
            <div className="mt-3 flex flex-col gap-2">
              {Array.from({ length: 6 }, (_, index) => (
                <Skeleton key={index} className="h-12 rounded-md" />
              ))}
            </div>
          ) : shownRows.length === 0 ? (
            <p className="mt-3 px-1 text-sm text-muted-foreground">No reminder with traces.</p>
          ) : (
            <ItemGroup className="mt-3 gap-1">
              {shownRows.map((row) => (
                <Item
                  key={row.reminderId}
                  asChild
                  variant={row.reminderId === selected ? 'muted' : 'outline'}
                  size="sm"
                >
                  <button
                    type="button"
                    onClick={() => setSelected(row.reminderId)}
                    data-testid="trace-reminder"
                    className="w-full text-left"
                  >
                    <ItemContent>
                      <ItemTitle className="w-full truncate">{row.text}</ItemTitle>
                      <ItemDescription>
                        #{row.reminderId} · {row.status} · {row.traces} rows
                      </ItemDescription>
                    </ItemContent>
                    <ItemActions>
                      <span aria-hidden="true" className="text-muted-foreground">
                        ›
                      </span>
                    </ItemActions>
                  </button>
                </Item>
              ))}
            </ItemGroup>
          )}
        </aside>

        <section className="min-w-0 flex-1 overflow-y-auto p-3 md:p-5">
          {failure && (
            <Alert variant="destructive" data-testid="trace-error">
              <AlertDescription className="text-destructive">{failure}</AlertDescription>
            </Alert>
          )}

          {selected === null ? (
            <p className="px-1 text-sm text-muted-foreground">
              Pick a reminder to see its history.
            </p>
          ) : current === null ? (
            <div className="flex flex-col gap-2">
              <Skeleton className="h-20 rounded-xl" />
              <Skeleton className="h-32 rounded-xl" />
            </div>
          ) : (
            <Card className="gap-4 bg-card/60 p-4 md:p-5">
              <CardHeader className="gap-1">
                <div className="flex flex-wrap items-center gap-2">
                  <CardTitle className="text-lg">{current.reminder.text}</CardTitle>
                  <Badge variant="outline" className="text-[10px]">
                    #{current.reminder.reminderId}
                  </Badge>
                  <Badge variant="secondary" className="text-[10px]">
                    {current.reminder.status}
                  </Badge>
                </div>
                <CardDescription>
                  {current.reminder.timezone} · {current.traces.length} rows · newest last
                </CardDescription>
              </CardHeader>

              <CardContent className="gap-3">
                {groups.length === 0 ? (
                  <p className="text-sm text-muted-foreground">Nothing recorded for this reminder.</p>
                ) : (
                  groups.map((group) => (
                    <section key={group.jobId} className="flex flex-col gap-1.5">
                      <div className="flex items-center gap-2">
                        <Badge variant="outline" className="font-mono text-[10px]">
                          job {group.jobId}
                        </Badge>
                        <span className="text-xs text-muted-foreground">
                          {group.traces.length === 1 ? '1 stage' : `${group.traces.length} stages`}
                        </span>
                      </div>
                      {group.traces.map((trace) => (
                        <TraceRow
                          key={trace.traceId}
                          trace={trace}
                          slowest={slowest}
                          onOpen={setDetail}
                        />
                      ))}
                    </section>
                  ))
                )}
              </CardContent>
            </Card>
          )}
        </section>
      </div>

      <Sheet open={detail !== null} onOpenChange={(open) => !open && setDetail(null)}>
        <SheetContent>
          {detail && (
            <>
              <SheetHeader>
                <SheetTitle>
                  <StageBadge stage={detail.stage} />
                  job {detail.jobId}
                </SheetTitle>
                <SheetDescription>
                  {clockOf(detail.timestamp)} · {msOf(detail)}ms
                </SheetDescription>
              </SheetHeader>
              <DetailFacts detail={detail.detail} />
            </>
          )}
        </SheetContent>
      </Sheet>
    </main>
  )
}
