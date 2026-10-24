import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import questionnaire from '@/data/questionnaire/v1.json'
import { api } from '@/lib/api'
import { setupPush, type PushSetup } from '@/lib/push'
import { useUser } from '@/lib/user'
import { cn } from '@/lib/utils'

const pushLabel: Record<PushSetup, string> = {
  ready: 'Notifications on',
  pending: 'Notifications pending',
  denied: 'Notifications blocked',
  unsupported: 'Notifications unsupported',
}

const pushDot: Record<PushSetup, string> = {
  ready: 'bg-emerald-400',
  pending: 'bg-primary',
  denied: 'bg-destructive',
  unsupported: 'bg-muted-foreground',
}

export default function Home() {
  const navigate = useNavigate()
  const { user, setUser } = useUser()
  const [push, setPush] = useState<PushSetup>('pending')

  // Setup is not done until the current questionnaire version is finished.
  useEffect(() => {
    if (user && user.lastQuestionnaireVersion < questionnaire.version) {
      navigate('/questionnaire', { replace: true })
    }
  }, [user, navigate])

  // Retry the push handoff on every load: the browser subscription may
  // already exist from a previous visit while the server was not ready.
  useEffect(() => {
    let live = true
    void setupPush({ prompt: false }).then((state) => {
      if (live) setPush(state)
    })
    return () => {
      live = false
    }
  }, [])

  async function signOut() {
    try {
      await api<void>('/api/auth/logout', { method: 'POST' })
    } catch (err) {
      // The local session is gone either way; a failed revoke must not trap
      // the user on Home.
      console.warn('Blurt: logout request failed', err)
    } finally {
      setUser(null)
      navigate('/login', { replace: true })
    }
  }

  return (
    <div className="flex min-h-dvh flex-col md:grid md:grid-cols-[270px_1fr]">
      <aside className="flex items-center gap-3 border-b border-border bg-black/20 px-4 py-3 md:flex-col md:items-stretch md:gap-2 md:border-b-0 md:border-r md:p-5">
        <div className="flex items-center gap-2 md:mb-4">
          <div className="relative size-7 rounded-lg bg-gradient-to-br from-[#ffcb6b] to-[#d9861c]">
            <span className="absolute inset-[27%_37%] rounded-full bg-[#241a08]" />
          </div>
          <span className="font-bold tracking-wide">Blurt</span>
        </div>
        <h2 className="hidden text-xs uppercase tracking-[0.12em] text-muted-foreground md:block">Waiting on you</h2>
        <p className="hidden text-sm text-muted-foreground md:block">Clarification threads show up here.</p>
        <div
          data-testid="push-status"
          className="ml-auto flex items-center gap-2 text-xs text-muted-foreground md:ml-0 md:mt-auto md:border-t md:border-border md:pt-4"
        >
          <span className={cn('size-2 rounded-full', pushDot[push])} aria-hidden="true" />
          {pushLabel[push]}
        </div>
      </aside>

      <main className="flex flex-col gap-8 px-4 py-6 md:px-10 md:py-8">
        <header className="flex items-start justify-between gap-4">
          <div>
            <h1 data-testid="home-heading" className="text-2xl font-semibold tracking-tight md:text-3xl">
              Hi {user?.name || 'there'}
            </h1>
            <p className="mt-1 text-sm text-muted-foreground">{user?.email}</p>
          </div>
          <Button variant="ghost" size="sm" onClick={() => void signOut()}>
            Sign out
          </Button>
        </header>

        <section className="my-auto flex w-full max-w-xl flex-col gap-3">
          <p className="text-xs uppercase tracking-[0.16em] text-primary">Ready when you are</p>
          <h2 className="text-2xl font-semibold tracking-tight md:text-4xl">Tell Blurt what to remember.</h2>
          <div className="flex flex-col gap-3 rounded-xl border border-border bg-card/80 p-3">
            <Textarea placeholder="Remind me to…" rows={2} aria-label="Blurt text input" />
            <div className="flex items-center justify-between">
              <Button
                type="button"
                variant="secondary"
                size="icon"
                disabled
                title="Voice capture lands next"
                aria-label="Record"
              >
                <span className="relative block h-6 w-4" aria-hidden="true">
                  <span className="absolute left-1 top-0 h-3.5 w-2 rounded-full bg-primary" />
                  <span className="absolute left-0 top-1.5 h-3 w-4 rounded-b-[10px] border-2 border-t-0 border-primary" />
                </span>
              </Button>
              <Button type="button" disabled>
                Send
              </Button>
            </div>
          </div>
          <p className="text-xs text-muted-foreground">Conversation and voice capture land next.</p>
        </section>
      </main>
    </div>
  )
}
