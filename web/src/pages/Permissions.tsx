import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { markPermissionsChecked, requestMicrophone, type PermissionState } from '@/lib/permissions'
import { setupPush, type PushSetup } from '@/lib/push'
import { cn } from '@/lib/utils'

type Step = PermissionState | 'checking'
type Status = Step | PushSetup

// The four tones a row wears: green is done, amber still needs a tap, red is a
// browser-settings trip, grey is not applicable.
const tone = {
  good: 'border-emerald-500/30 text-emerald-300',
  wait: 'border-amber-500/30 text-amber-300',
  bad: 'border-destructive/40 text-destructive',
  idle: 'border-border/60 text-muted-foreground',
}

// One row of the table per state: the words, and the dot and pill that say the
// same thing without them.
const states: Record<Status, { label: string; dot: string; pill: string }> = {
  checking: { label: 'Checking…', dot: 'bg-primary animate-pulse', pill: tone.idle },
  granted: { label: 'Allowed', dot: 'bg-emerald-400', pill: tone.good },
  ready: { label: 'Allowed', dot: 'bg-emerald-400', pill: tone.good },
  prompt: { label: 'Not asked yet', dot: 'bg-amber-400', pill: tone.wait },
  pending: { label: 'Not asked yet', dot: 'bg-amber-400', pill: tone.wait },
  denied: { label: 'Blocked', dot: 'bg-destructive', pill: tone.bad },
  unsupported: { label: 'Unsupported in this browser', dot: 'bg-muted-foreground', pill: tone.idle },
}

// StatePill is the dot and the words together: one glance says whether the row
// is done, still needs a tap, or is a browser-settings trip.
function StatePill({ state, label }: { state: Status; label?: string }) {
  const { dot, pill, label: text } = states[state]
  return (
    <span className={cn('inline-flex items-center gap-1.5 rounded-full border px-2 py-1 text-xs', pill)}>
      <span className={cn('size-1.5 rounded-full', dot)} aria-hidden="true" />
      {label ?? text}
    </span>
  )
}

// Permissions is the gate the app runs on every entry, before the questionnaire.
// Both permissions can be revoked in browser settings at any time, so the only
// way to notice is to ask again as early as possible.
export default function Permissions() {
  const navigate = useNavigate()
  // This screen runs on every app entry, so a deep link from a notification
  // lands here first: whatever path the user asked for is where Continue goes.
  const from = (useLocation().state as { from?: string } | null)?.from ?? '/home'
  const [mic, setMic] = useState<Step>('checking')
  // The mount check runs setupPush, which can take a moment on a cold start, so
  // the row starts in the same "still working" state the microphone row uses.
  const [push, setPush] = useState<Status>('checking')
  const [pushBusy, setPushBusy] = useState(false)

  // Asked on mount: getUserMedia is the only call that can raise the browser's
  // microphone prompt, and on an already-granted permission it is silent.
  useEffect(() => {
    let live = true
    void requestMicrophone().then((state) => {
      if (live) setMic(state)
    })
    return () => {
      live = false
    }
  }, [])

  // Silent: this reads the current push state, it never prompts.
  useEffect(() => {
    let live = true
    void setupPush({ prompt: false }).then((state) => {
      if (live) setPush(state)
    })
    return () => {
      live = false
    }
  }, [])

  // Nothing to ask for: get out of the way. A returning user who has already
  // allowed both should not have to dismiss this screen.
  useEffect(() => {
    if (mic !== 'granted' || push !== 'ready') return
    markPermissionsChecked()
    navigate(from, { replace: true })
  }, [mic, push, navigate, from])

  // Browsers only prompt while a permission is unset. Once denied, the app can
  // do nothing but say so: the user has to change it in site settings.
  function askMic() {
    setMic('checking')
    void requestMicrophone().then(setMic)
  }

  // Turning on is a permission prompt, a key fetch and a subscription. The row
  // says so while it runs, and the button cannot be fired twice.
  function askPush() {
    setPushBusy(true)
    void setupPush({ prompt: true })
      .then(setPush)
      .finally(() => setPushBusy(false))
  }

  function continueOn() {
    markPermissionsChecked()
    navigate(from, { replace: true })
  }

  const micBlocked = mic === 'denied'
  const ready = (mic === 'granted' ? 1 : 0) + (push === 'ready' ? 1 : 0)
  // The notification row shows the busy state while the handoff runs.
  const shown: Status = pushBusy ? 'checking' : push
  const shownLabel = pushBusy ? 'Setting up…' : states[push].label

  return (
    <main className="grid min-h-dvh place-items-center px-4 py-8">
      <Card className="w-full max-w-md gap-6 bg-card/90 backdrop-blur">
        <CardHeader className="items-start gap-2">
          <CardTitle className="text-2xl tracking-tight">Two things to allow</CardTitle>
          <CardDescription>
            Blurt starts with your voice and sends its follow-ups as notifications. Both stay on this
            device.
          </CardDescription>
        </CardHeader>

        <CardContent className="flex flex-col gap-4">
          {/* One glance is enough: the bar fills as each permission lands. */}
          <div className="flex items-center gap-3" aria-live="polite">
            <div className="flex flex-1 gap-1" aria-hidden="true">
              <span
                className={cn(
                  'h-1.5 flex-1 rounded-full transition-colors',
                  mic === 'granted' ? 'bg-emerald-400' : 'bg-muted',
                )}
              />
              <span
                className={cn(
                  'h-1.5 flex-1 rounded-full transition-colors',
                  push === 'ready' ? 'bg-emerald-400' : 'bg-muted',
                )}
              />
            </div>
            <span className="text-xs tabular-nums text-muted-foreground">{ready} of 2 ready</span>
          </div>

          <Item
            data-testid="permissions-mic"
            variant={mic === 'granted' ? 'muted' : 'outline'}
            size="sm"
            className="items-start justify-between gap-3 p-3"
          >
            <ItemContent>
              <ItemTitle>Microphone</ItemTitle>
              <ItemDescription className="text-xs">
                {micBlocked
                  ? 'Blocked. Allow the microphone for this site in your browser settings, then try again.'
                  : 'So you can blurt without typing.'}
              </ItemDescription>
            </ItemContent>
            <ItemActions className="shrink-0 flex-wrap justify-end">
              <StatePill state={mic} />
              {(micBlocked || mic === 'prompt') && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  data-testid="permissions-mic-retry"
                  onClick={askMic}
                >
                  Try again
                </Button>
              )}
            </ItemActions>
          </Item>

          <Item
            data-testid="permissions-notifications"
            variant={push === 'ready' ? 'muted' : 'outline'}
            size="sm"
            className="items-start justify-between gap-3 p-3"
          >
            <ItemContent>
              <ItemTitle>Notifications</ItemTitle>
              <ItemDescription className="text-xs">
                {push === 'denied'
                  ? 'Blocked. Allow notifications for this site in your browser settings.'
                  : 'So Blurt can nudge you at the right time.'}
              </ItemDescription>
            </ItemContent>
            <ItemActions className="shrink-0 flex-wrap justify-end">
              <StatePill state={shown} label={shownLabel} />
              {(push === 'pending' || push === 'denied') && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  data-testid="permissions-notify-enable"
                  disabled={pushBusy}
                  onClick={askPush}
                >
                  {pushBusy ? 'Turning on…' : 'Turn on'}
                </Button>
              )}
            </ItemActions>
          </Item>

          <Button type="button" size="lg" data-testid="permissions-continue" onClick={continueOn}>
            Continue
          </Button>
          <p className="text-xs text-muted-foreground">
            You can change both later in your browser settings. Blurt will ask again when you return.
          </p>
        </CardContent>
      </Card>
    </main>
  )
}