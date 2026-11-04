import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { BackgroundPaths } from '@/components/BackgroundPaths'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { api } from '@/lib/api'
import { useUser, type UserEnvelope } from '@/lib/user'

// One question at a time: are you already in, and only then the field that
// question needs. Everything the app asks lives on this page, in sequence.
type Step = 'choice' | 'signin' | 'pin' | 'apply' | 'applied'

const copy: Record<Step, { title: string; description: string }> = {
  choice: { title: 'Welcome to Blurt', description: 'Do you already have a Blurt account?' },
  signin: { title: 'Sign in', description: 'We will email you a six-digit PIN. No password to remember.' },
  pin: { title: 'Check your email', description: '' },
  apply: { title: 'Request access', description: 'Blurt is invite-only. Tell us who you are and the owner will take a look.' },
  applied: { title: 'Application received', description: '' },
}

// installNote sits under either submit button: Blurt is used on a phone, and the
// approval email is the first thing that should open the installed app. Plain
// text, not a card — it is a footnote to the button above it.
const installNote = (
  <div className="flex flex-col gap-1 text-xs leading-relaxed text-muted-foreground" data-testid="install-note">
    <p>Put Blurt on your iPhone&rsquo;s home screen:</p>
    <ol className="list-decimal space-y-0.5 pl-4">
      <li>
        Open Blurt in <span className="text-foreground">Safari</span>.
      </li>
      <li>
        Tap the <span className="text-foreground">Share</span> button.
      </li>
      <li>
        Tap <span className="text-foreground">Add to Home Screen</span>.
      </li>
    </ol>
    <a
      href="https://support.apple.com/en-my/guide/iphone/iphea86e5236/ios"
      target="_blank"
      rel="noreferrer"
      className="w-fit underline underline-offset-4 hover:text-foreground"
    >
      Apple&rsquo;s step-by-step guide
    </a>
  </div>
)

export default function Login() {
  const navigate = useNavigate()
  const { user, setUser } = useUser()
  // A notification deep link that arrived with an expired session resumes
  // here instead of dropping the user on Home.
  const raw = (useLocation().state as { from?: string } | null)?.from
  const from = raw?.startsWith('/') && !raw.startsWith('//') ? raw : '/home'
  const [step, setStep] = useState<Step>('choice')
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [pin, setPin] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Already signed in (session cookie still valid): skip the form.
  useEffect(() => {
    if (user) navigate(from, { replace: true })
  }, [user, navigate, from])

  function go(next: Step) {
    setError(null)
    setBusy(false)
    setStep(next)
  }

  async function requestPin(event: FormEvent) {
    event.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await api('/api/auth/request-pin', {
        method: 'POST',
        body: JSON.stringify({ email: email.trim() }),
      })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not send the PIN.')
      setBusy(false)
      return
    }
    setStep('pin')
    setBusy(false)
  }

  async function verifyPin(event: FormEvent) {
    event.preventDefault()
    setError(null)
    setBusy(true)
    let signedIn: UserEnvelope
    try {
      signedIn = await api<UserEnvelope>('/api/auth/verify-pin', {
        method: 'POST',
        body: JSON.stringify({ email: email.trim(), pin }),
      })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'That PIN did not work.')
      setBusy(false)
      return
    }

    setUser(signedIn.user)
    // The permission gate (App.tsx) takes over from here and runs ahead of the
    // questionnaire, so signing in and asking for permissions stay separate.
    navigate(from, { replace: true })
  }

  async function apply(event: FormEvent) {
    event.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await api('/api/auth/apply', {
        method: 'POST',
        body: JSON.stringify({ name: name.trim(), email: email.trim() }),
      })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not send the application.')
      setBusy(false)
      return
    }
    setBusy(false)
    setStep('applied')
  }

  const description =
    step === 'pin' ? (
      <>
        PIN sent to <strong className="break-all text-foreground">{email}</strong>. It expires in five minutes.
      </>
    ) : step === 'applied' ? (
      <>
        Thanks, {name.trim().split(' ')[0] || 'there'}. We will email{' '}
        <strong className="break-all text-foreground">{email}</strong> the moment your account is approved.
      </>
    ) : (
      copy[step].description
    )

  return (
    <main className="relative isolate flex min-h-dvh flex-col px-5 pb-6 pt-[max(1.5rem,env(safe-area-inset-top))] sm:px-8">
      <BackgroundPaths scrim={false} />
      <div className="relative mx-auto flex w-full max-w-sm flex-1 flex-col justify-center gap-8 py-8">
        <Link to="/" className="flex items-center gap-2.5 self-start" data-testid="login-home">
          <span className="relative inline-block size-9 rounded-2xl bg-gradient-to-br from-[#ffcb6b] to-[#d9861c]">
            <span className="absolute inset-[27%_37%] rounded-full bg-[#241a08]" />
          </span>
          <span className="text-lg font-semibold tracking-tight">Blurt</span>
        </Link>

        <Card className="gap-6 bg-card/90 backdrop-blur">
          <CardHeader className="gap-2">
            <CardTitle className="text-2xl font-semibold tracking-tight" data-testid="login-title">
              {copy[step].title}
            </CardTitle>
            <CardDescription className="text-sm leading-relaxed">{description}</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            {step === 'choice' && (
              <div className="flex flex-col gap-2">
                <Button
                  type="button"
                  size="lg"
                  className="h-auto min-h-12 justify-start py-3 text-base"
                  data-testid="login-have-account"
                  onClick={() => go('signin')}
                >
                  Yes, I have an account
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="lg"
                  className="h-auto min-h-12 justify-start py-3 text-base"
                  data-testid="login-apply"
                  onClick={() => go('apply')}
                >
                  No, I need access
                </Button>
              </div>
            )}

            {step === 'signin' && (
              <form onSubmit={requestPin} className="flex flex-col gap-4">
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="email">Email</Label>
                  <Input
                    id="email"
                    data-testid="login-email"
                    type="email"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                    autoFocus
                    required
                  />
                </div>
                <Button
                  type="submit"
                  size="lg"
                  data-testid="login-submit"
                  className="h-auto min-h-11 whitespace-normal py-2 leading-tight"
                  disabled={busy || !email.trim()}
                >
                  {busy ? 'Sending PIN…' : 'Email me a PIN'}
                </Button>
                {installNote}
                <Button type="button" variant="ghost" size="sm" onClick={() => go('choice')}>
                  Back
                </Button>
              </form>
            )}

            {step === 'pin' && (
              <form onSubmit={verifyPin} className="flex flex-col gap-4">
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="pin">Login PIN</Label>
                  <Input
                    id="pin"
                    data-testid="login-pin"
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    pattern="[0-9]*"
                    maxLength={6}
                    value={pin}
                    onChange={(e) => setPin(e.target.value.replace(/\D/g, ''))}
                    placeholder="123456"
                    autoFocus
                    required
                  />
                </div>
                <Button
                  type="submit"
                  size="lg"
                  data-testid="login-verify"
                  className="h-auto min-h-11 whitespace-normal py-2 leading-tight"
                  disabled={busy || pin.length < 6}
                >
                  {busy ? 'Checking…' : 'Enter Blurt'}
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    setPin('')
                    go('signin')
                  }}
                >
                  Use a different email
                </Button>
              </form>
            )}

            {step === 'apply' && (
              <form onSubmit={apply} className="flex flex-col gap-4">
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="name">Your name</Label>
                  <Input
                    id="name"
                    data-testid="login-name"
                    autoComplete="name"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="Lam"
                    autoFocus
                    required
                  />
                </div>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="email">Email</Label>
                  <Input
                    id="email"
                    data-testid="login-email"
                    type="email"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                    required
                  />
                </div>
                <Button
                  type="submit"
                  size="lg"
                  data-testid="apply-submit"
                  className="h-auto min-h-11 whitespace-normal py-2 leading-tight"
                  disabled={busy || !name.trim() || !email.trim()}
                >
                  {busy ? 'Sending…' : 'Request access'}
                </Button>
                {installNote}
                <Button type="button" variant="ghost" size="sm" onClick={() => go('choice')}>
                  Back
                </Button>
              </form>
            )}

            {step === 'applied' && (
              <Button
                type="button"
                variant="outline"
                size="lg"
                data-testid="applied-back"
                className="h-auto min-h-11 py-2"
                onClick={() => go('choice')}
              >
                Back to sign in
              </Button>
            )}

            {error && (
              <Alert variant="destructive">
                <AlertDescription className="break-words text-destructive">{error}</AlertDescription>
              </Alert>
            )}
          </CardContent>
        </Card>
      </div>
    </main>
  )
}
