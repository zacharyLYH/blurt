import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { BackgroundPaths } from '@/components/BackgroundPaths'
import { PhoneShowcase } from '@/components/PhoneShowcase'
import { Badge } from '@/components/ui/badge'
import { buttonVariants } from '@/components/ui/button-variants'

// What people actually say to Blurt. The carousel is the product's pitch in the
// user's own words, so it stays in the imperative.
const phrases = [
  'remind me to take my meds',
  'remind me to call my mum',
  'remind me to pay the rent',
  'remind me to book the dentist',
  'remind me to water the plants',
  'remind me to leave for the airport',
]

// The wordmark's green, glowing. The hero and the microtask page have to be the
// same shade, so the classes live in one place.
const neonGlow = 'text-neon drop-shadow-[0_0_45px_rgba(120,255,120,0.35)]'

// The wordmark, drawn once: the amber capsule is the product, the neon is the
// page it is being sold on.
function Mark({ className }: { className?: string }) {
  return (
    <span className={`relative inline-block rounded-2xl bg-gradient-to-br from-[#ffcb6b] to-[#d9861c] ${className}`}>
      <span className="absolute inset-[27%_37%] rounded-full bg-[#241a08]" />
    </span>
  )
}

// Landing is the one public page. It never looks at the session: a returning
// user who follows a marketing link stays here until they choose to sign in.
export default function Landing() {
  const [phrase, setPhrase] = useState(0)

  useEffect(() => {
    const timer = window.setInterval(() => setPhrase((current) => (current + 1) % phrases.length), 2600)
    return () => window.clearInterval(timer)
  }, [])

  return (
    <main className="relative isolate flex min-h-dvh flex-col overflow-hidden" data-testid="landing">
      <BackgroundPaths />

      <header className="relative z-10 flex items-center justify-between px-5 pb-5 pt-[max(1.25rem,env(safe-area-inset-top))] sm:px-8">
        <div className="flex items-center gap-2.5">
          <Mark className="size-8" />
          <span className="text-lg font-semibold tracking-tight">Blurt</span>
        </div>
        <Link to="/login" className={buttonVariants({ variant: 'ghost', size: 'sm' })}>
          Sign in
        </Link>
      </header>

      {/* The hero owns the first screen on a phone; the phone mock-up is the
          second page, so neither has to be squeezed. */}
      <section className="relative z-10 mx-auto flex w-full max-w-6xl flex-1 flex-col items-center justify-center gap-12 px-5 pb-16 pt-6 text-center sm:px-8 lg:flex-row lg:items-center lg:justify-between lg:gap-10 lg:text-left">
        <div className="flex w-full max-w-xl flex-col items-center gap-6 lg:items-start">
          <Badge variant="outline" className="border-neon/30 bg-neon/10 text-neon">
            Private beta · apply for access
          </Badge>

          <h1 className={`text-6xl leading-none font-black tracking-tighter sm:text-7xl lg:text-8xl ${neonGlow}`}>
            Blurt
          </h1>

          <div className="flex min-h-20 w-full flex-col items-center gap-2 lg:items-start">
            <span className="text-sm font-medium tracking-wide text-muted-foreground uppercase">
              Say it once
            </span>
            <p
              key={phrase}
              className="blurt-phrase text-2xl font-bold leading-tight text-balance sm:text-3xl"
              data-testid="landing-phrase"
            >
              “{phrases[phrase]}”
            </p>
          </div>

          <p className="max-w-md text-base leading-relaxed text-balance text-muted-foreground">
            Blurt listens, asks when it is unsure, and turns what you said into reminders that
            arrive when they matter — not a list you have to maintain.
          </p>

          <div className="flex flex-col items-center gap-3 sm:flex-row lg:items-start">
            <Link to="/login" className={buttonVariants({ size: 'lg', className: 'min-w-44 text-base' })}>
              Get early access
            </Link>
            <span className="text-sm text-muted-foreground">Invite-only. We answer every application.</span>
          </div>
        </div>

        <PhoneShowcase className="hidden lg:flex" />
      </section>

      {/* The pitch's other half, on a page of its own: Blurt is for what is too
          small to earn a calendar entry, so the calendar someone already keeps
          stays theirs. The two green words are the whole idea — say it, to
          Blurt — and the line beneath them is the boundary. */}
      <section className="relative z-10 flex min-h-dvh flex-col justify-center border-t border-border/40 px-5 py-20 sm:px-8">
        <div className="mx-auto flex w-full max-w-5xl flex-col gap-8">
          <span className="text-xs font-medium tracking-[0.25em] text-muted-foreground uppercase">
            The small stuff
          </span>
          <h2 className="text-5xl leading-[0.94] font-black tracking-tighter text-balance sm:text-7xl lg:text-8xl">
            Stop scheduling microtasks.{' '}
            <span className={neonGlow}>Say</span> them to <span className={neonGlow}>Blurt</span>.
          </h2>
          <p className="max-w-xl text-base leading-relaxed text-muted-foreground sm:text-lg">
            Keep your calendar app for the big important things like work and school. 
            But for the everyday friction and million things you need to remember, just Blurt 🗣️
          </p>
        </div>
      </section>

      {/* A phone has no room beside the hero, so the shot gets its own page. */}
      <section className="relative z-10 border-t border-border/40 px-5 py-14 lg:hidden">
        <PhoneShowcase />
      </section>
    </main>
  )
}