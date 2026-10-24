import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { markPermissionsChecked, requestMicrophone, type PermissionState } from '@/lib/permissions'
import { setupPush, type PushSetup } from '@/lib/push'

type Step = PermissionState | 'checking'

const stateLabel: Record<Step, string> = {
  checking: 'Checking…',
  granted: 'Allowed',
  prompt: 'Not asked yet',
  denied: 'Blocked',
  unsupported: 'Unsupported in this browser',
}

const pushLabel: Record<PushSetup, string> = {
  ready: 'Allowed',
  pending: 'Not asked yet',
  denied: 'Blocked',
  unsupported: 'Unsupported in this browser',
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
  const [push, setPush] = useState<PushSetup>('pending')

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

  function continueOn() {
    markPermissionsChecked()
    navigate(from, { replace: true })
  }

  const micBlocked = mic === 'denied'

  return (
    <main className="grid min-h-dvh place-items-center px-4 py-8">
      <Card className="w-full max-w-md gap-6 bg-card/90 backdrop-blur">
        <CardHeader className="items-start gap-2">
          <CardTitle className="text-2xl tracking-tight">Two things to allow</CardTitle>
          <CardDescription>
            Blurt is voice-first and sends its questions as notifications. Both stay on this device.
          </CardDescription>
        </CardHeader>

        <CardContent className="flex flex-col gap-4">
          <Item
            data-testid="permissions-mic"
            variant="outline"
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
            <ItemActions className="shrink-0">
              <span className="text-xs text-muted-foreground">{stateLabel[mic]}</span>
              {(micBlocked || mic === 'prompt') && (
                <Button type="button" variant="ghost" size="sm" data-testid="permissions-mic-retry" onClick={askMic}>
                  Try again
                </Button>
              )}
            </ItemActions>
          </Item>

          <Item
            data-testid="permissions-notifications"
            variant="outline"
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
            <ItemActions className="shrink-0">
              <span className="text-xs text-muted-foreground">{pushLabel[push]}</span>
              {(push === 'pending' || push === 'denied') && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  data-testid="permissions-notify-enable"
                  onClick={() => void setupPush({ prompt: true }).then(setPush)}
                >
                  Turn on
                </Button>
              )}
            </ItemActions>
          </Item>

          <Button type="button" size="lg" data-testid="permissions-continue" onClick={continueOn}>
            Continue
          </Button>
          <p className="text-xs text-muted-foreground">
            You can continue without them; Blurt falls back to typing and keeps its answers in the app.
          </p>
        </CardContent>
      </Card>
    </main>
  )
}