import { Link, useNavigate } from 'react-router-dom'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import type { JobSummary } from '@/lib/jobs'
import type { PushStatus } from '@/lib/push'
import { useUser } from '@/lib/user'
import { cn } from '@/lib/utils'

const pushCopy: Record<PushStatus, { label: string; dot: string }> = {
  checking: { label: 'Setting up…', dot: 'bg-primary animate-pulse' },
  ready: { label: 'Notifications on', dot: 'bg-emerald-400' },
  pending: { label: 'Notifications pending', dot: 'bg-primary' },
  denied: { label: 'Notifications blocked', dot: 'bg-destructive' },
  unsupported: { label: 'Notifications unsupported', dot: 'bg-muted-foreground' },
}

// The zone the calendar is drawn in, so a reminder's own clock has something to
// be read against. Short name in the header, IANA name in the tooltip.
function timezoneLabel(zone: string): string {
  const parts = new Intl.DateTimeFormat(undefined, { timeZoneName: 'short' }).formatToParts(new Date())
  return parts.find((part) => part.type === 'timeZoneName')?.value ?? zone
}

// IconButton is one glyph in the header. The strokes are drawn once here so
// every button is the same size and the SVG boilerplate stays in one place.
function IconButton({
  label,
  testId,
  strokeWidth = 1.8,
  onClick,
  children,
}: {
  label: string
  testId?: string
  strokeWidth?: number
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      aria-label={label}
      title={label}
      data-testid={testId}
      onClick={onClick}
      className="size-8 shrink-0"
    >
      <svg
        viewBox="0 0 24 24"
        className="size-4"
        fill="none"
        stroke="currentColor"
        strokeWidth={strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        {children}
      </svg>
    </Button>
  )
}

interface AppShellProps {
  // inbox is every thread that wants an answer. A conversation is reachable only
  // through its row, so this list never promises to hold every chat.
  inbox: JobSummary[]
  push: PushStatus
  onSignOut: () => void
  // Opening the inbox is the moment to re-ask.
  onOpenInbox: () => void
  children: React.ReactNode
}

// AppShell is the one frame the calendar shares: the inbox, the way to the
// reminder list, and the way out.
export function AppShell({ inbox, push, onSignOut, onOpenInbox, children }: AppShellProps) {
  const navigate = useNavigate()
  const { user } = useUser()
  const owner = user?.owner === true
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
  const { label: pushLabel, dot: pushDot } = pushCopy[push]

  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <header className="flex shrink-0 items-center gap-1.5 border-b border-border/60 bg-black/20 px-2 pb-2 pt-[max(0.75rem,env(safe-area-inset-top))]">
        <DropdownMenu
          onOpenChange={(next) => {
            // Opening is the moment to re-ask: a question that landed while the
            // menu was shut is already there when it opens.
            if (next) onOpenInbox()
          }}
        >
          <DropdownMenuTrigger asChild>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              data-testid="inbox-toggle"
              aria-label="Inbox"
              className="relative size-8 shrink-0"
            >
              <svg
                viewBox="0 0 24 24"
                className="size-5"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="M4 13h4l1.5 2.5h5L16 13h4M4 13l2-8h12l2 8v6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z" />
              </svg>

              {inbox.length > 0 && (
                <Badge className="absolute -right-0.5 -top-0.5 size-4 justify-center rounded-full p-0 text-[9px] tabular-nums motion-safe:animate-pulse">
                  {inbox.length}
                </Badge>
              )}
            </Button>
          </DropdownMenuTrigger>

          <DropdownMenuContent
            data-testid="inbox"
            align="start"
            className="w-64 max-w-[calc(100vw-2rem)]"
          >
            {inbox.length === 0 ? (
              <p
                data-testid="inbox-empty"
                className="px-2 py-1.5 text-sm text-muted-foreground"
              >
                Nothing needs you.
              </p>
            ) : (
              inbox.map((job) => (
                <DropdownMenuItem key={job.jobId} asChild>
                  <Link
                    to={`/jobs/${job.jobId}`}
                    data-testid="inbox-item"
                    className="justify-between"
                  >
                    <span className="truncate">{job.preview || 'Voice clip'}</span>
                    <Badge variant="secondary" className="text-[10px]">
                      Question
                    </Badge>
                  </Link>
                </DropdownMenuItem>
              ))
            )}
          </DropdownMenuContent>
        </DropdownMenu>

        <Button
          type="button"
          variant="ghost"
          onClick={() => navigate('/home')}
          className="gap-2 px-1"
        >
          <span className="relative size-6 rounded-md bg-gradient-to-br from-[#ffcb6b] to-[#d9861c]">
            <span className="absolute inset-[27%_37%] rounded-full bg-[#241a08]" />
          </span>
          <span className="text-sm font-bold tracking-wide">Blurt</span>
        </Button>

        <Badge
          data-testid="push-status"
          variant="outline"
          title={pushLabel}
          aria-label={pushLabel}
          className="ml-auto min-w-0 gap-1.5 bg-card/60 px-2 text-[10px] font-normal text-muted-foreground"
        >
          <span
            className={cn('size-1.5 shrink-0 rounded-full', pushDot)}
            aria-hidden="true"
          />
          {/* The zone and the two icons need this row's width on a phone, so the
              pill keeps only its dot there and shows the words from sm up. */}
          <span className="hidden truncate sm:inline">{pushLabel}</span>
        </Badge>

        <Badge
          data-testid="timezone"
          variant="outline"
          title={zone}
          className="bg-card/60 px-2 text-[10px] font-normal text-muted-foreground"
        >
          {timezoneLabel(zone)}
        </Badge>

        {owner && (
          <IconButton label="Traces" testId="shell-traces" onClick={() => navigate('/traces')}>
            <circle cx="6" cy="6" r="2.5" />
            <circle cx="6" cy="18" r="2.5" />
            <circle cx="18" cy="12" r="2.5" />
            <path d="M8.5 6h4a3 3 0 0 1 3 3v.5M8.5 18h4a3 3 0 0 0 3-3v-.5" />
          </IconButton>
        )}

        {/* The portal is the owner's: approving people is what they are for. */}
        {owner && (
          <IconButton label="Access requests" testId="shell-admin" onClick={() => navigate('/admin')}>
            <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
            <circle cx="9" cy="7" r="4" />
            <path d="M19 8v6M22 11h-6" />
          </IconButton>
        )}

        <IconButton label="Sign out" strokeWidth={2} onClick={onSignOut}>
          <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9" />
        </IconButton>
      </header>

      {children}
    </div>
  )
}
