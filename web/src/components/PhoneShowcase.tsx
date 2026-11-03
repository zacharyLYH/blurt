import { useEffect, useState } from 'react'
import { cn } from '@/lib/utils'

// The app's own screens, captured at phone size. They are the proof the hero
// only promises: this is what saying it once actually looks like.
const shots = [
  { src: '/shots/home.png', label: 'A month of reminders' },
  { src: '/shots/voice.png', label: 'Say it instead of typing it' },
  { src: '/shots/inbox.png', label: 'Questions when Blurt is unsure' },
]

// PhoneShowcase is an iPhone with the product inside it, turning over every few
// seconds. One instance sits in the hero on a wide screen and one below it on a
// phone, so the shot is never squeezed into a column it does not fit.
export function PhoneShowcase({ className }: { className?: string }) {
  const [shown, setShown] = useState(0)

  useEffect(() => {
    const timer = window.setInterval(() => setShown((current) => (current + 1) % shots.length), 3200)
    return () => window.clearInterval(timer)
  }, [])

  return (
    <div className={cn('flex flex-col items-center gap-4', className)} data-testid="landing-phone">
      <div className="blurt-float relative rounded-[2.75rem] border-[10px] border-[#0b0b0f] bg-[#0b0b0f] shadow-[0_40px_120px_-30px_rgba(0,0,0,0.9)] ring-1 ring-white/10">
        {/* The island, so the frame reads as a phone rather than a card. */}
        <div className="absolute left-1/2 top-3 z-10 h-5 w-24 -translate-x-1/2 rounded-full bg-black" />
        <div className="relative aspect-[390/844] w-[248px] overflow-hidden rounded-[2rem] bg-background sm:w-[272px]">
          {shots.map((shot, index) => (
            <img
              key={shot.src}
              src={shot.src}
              alt={shot.label}
              width={390}
              height={844}
              className={cn(
                'absolute inset-0 size-full object-cover transition-opacity duration-700',
                index === shown ? 'opacity-100' : 'opacity-0',
              )}
            />
          ))}
        </div>
      </div>

      <p className="h-4 text-xs text-muted-foreground" data-testid="landing-phone-caption">
        {shots[shown].label}
      </p>
    </div>
  )
}