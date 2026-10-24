import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { api } from '@/lib/api'
import { useUser, type UserEnvelope } from '@/lib/user'

type Step = 'identify' | 'pin'

export default function Login() {
  const navigate = useNavigate()
  const { user, setUser } = useUser()
  const [step, setStep] = useState<Step>('identify')
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [pin, setPin] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Already signed in (session cookie still valid): skip the form.
  useEffect(() => {
    if (user) navigate('/home', { replace: true })
  }, [user, navigate])

  async function requestPin(event: FormEvent) {
    event.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await api('/api/auth/request-pin', {
        method: 'POST',
        body: JSON.stringify({ name: name.trim(), email: email.trim() }),
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
        body: JSON.stringify({ name: name.trim(), email: email.trim(), pin }),
      })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'That PIN did not work.')
      setBusy(false)
      return
    }

    setUser(signedIn.user)
    // The permission gate (App.tsx) takes over from here and runs ahead of the
    // questionnaire, so signing in and asking for permissions stay separate.
    navigate('/home', { replace: true })
  }

  return (
    <main className="grid min-h-dvh place-items-center px-4 py-8">
      <Card className="w-full max-w-sm gap-6 bg-card/90 backdrop-blur">
        <CardHeader className="items-start gap-2">
          <div className="relative size-9 rounded-[11px] bg-gradient-to-br from-[#ffcb6b] to-[#d9861c] shadow-inner">
            <span className="absolute inset-[27%_37%] rounded-full bg-[#241a08]" />
          </div>
          <CardTitle className="text-2xl tracking-tight">Blurt</CardTitle>
          <CardDescription>Say it. It becomes a reminder or a memory.</CardDescription>
        </CardHeader>

        <CardContent>
          {step === 'identify' ? (
            <form onSubmit={requestPin} className="flex flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="name">Your name</Label>
                <Input
                  id="name"
                  data-testid="login-name"
                  autoComplete="name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="Lam"
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
                data-testid="login-submit"
                disabled={busy || !name.trim() || !email.trim()}
              >
                {busy ? 'Sending PIN…' : 'Email me a PIN'}
              </Button>
            </form>
          ) : (
            <form onSubmit={verifyPin} className="flex flex-col gap-4">
              <p className="text-sm text-muted-foreground">
                PIN sent to <strong className="text-foreground">{email}</strong>. It expires in five minutes.
              </p>
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
              <Button type="submit" size="lg" data-testid="login-verify" disabled={busy || pin.length < 6}>
                {busy ? 'Checking…' : 'Enter Blurt'}
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => {
                  setStep('identify')
                  setPin('')
                  setError(null)
                }}
              >
                Use a different email
              </Button>            </form>
          )}

          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
        </CardContent>
      </Card>
    </main>
  )
}
