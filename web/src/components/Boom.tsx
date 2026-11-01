import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import type { MicSession } from '@/lib/recorder'
import { cn } from '@/lib/utils'

interface BoomProps {
  session: MicSession | null
  recording: boolean
  remainingMs: number
  onRecord: () => void
  onCancel: () => void
  onSend: () => void
  // fullscreen turns the control into the whole page: the rings are the screen,
  // and only the two clip decisions sit under them.
  fullscreen?: boolean
}

// How many pulses are in the air at once. Each one is a quarter cycle behind
// the next, so the rings read as a wave leaving the button rather than a
// single plate breathing.
const RING_COUNT = 4

// Boom is the capture control: a wave that answers the microphone and the two
// decisions a clip needs — keep it or throw it away.
export function Boom({
  session,
  recording,
  remainingMs,
  onRecord,
  onCancel,
  onSend,
  fullscreen = false,
}: BoomProps) {
  const [wave, setWave] = useState({ level: 0, phase: 0 })

  // Sampled per frame while the control is on screen. `level` is the smoothed
  // loudness — rising fast and falling slowly is what reads as a breath rather
  // than a flicker. `phase` counts the pulses emitted so far, so a loud room
  // emits them faster and carries them further, which is the only thing that
  // makes the wave feel like it belongs to the room.
  useEffect(() => {
    if (!session) return
    const still = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    let frame = 0
    let last = performance.now()
    let smoothed = 0
    let phase = 0
    const tick = (now: number) => {
      const reading = recording ? session.loudness() : 0
      smoothed = reading > smoothed ? reading : smoothed * 0.88
      // 2.2s per pulse in a quiet room, 1.1s at full voice.
      if (!still) phase += (now - last) / 1000 / (2.2 - smoothed * 1.1)
      last = now
      setWave({ level: smoothed, phase })
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [recording, session])

  const secondsLeft = Math.max(0, Math.ceil(remainingMs / 1000))

  return (
    <div className={cn('flex flex-col items-center', fullscreen ? 'gap-10' : 'gap-5')}>
      <div
        data-testid="boom"
        className={cn('relative grid place-items-center', fullscreen ? 'size-[min(80vw,70vh)]' : 'size-52')}
      >
        {/* The pulses start behind the button — too small to see — and grow past
            its edge as a ring of light, brightest when they are newest.
            Loudness decides how fast they leave, how far they carry, and how
            hard they burn: a whisper stays a ripple at the button, a shout
            crosses the screen. */}
        {Array.from({ length: RING_COUNT }, (_, ring) => {
          const progress = (wave.phase + ring / RING_COUNT) % 1
          return (
            <span
              key={ring}
              aria-hidden="true"
              className="absolute left-1/2 top-1/2 size-full rounded-full border-primary"
              style={{
                borderWidth: fullscreen ? 3 : 2,
                transform: `translate(-50%, -50%) scale(${0.2 + progress * (0.5 + wave.level * 0.95)})`,
                opacity: (1 - progress) ** 1.4 * (0.4 + wave.level * 0.6),
              }}
            />
          )
        })}
        <span
          aria-hidden="true"
          className="pointer-events-none absolute left-1/2 top-1/2 size-32 rounded-full bg-primary blur-2xl"
          style={{
            transform: `translate(-50%, -50%) scale(${1 + wave.level * 0.6})`,
            opacity: 0.2 + wave.level * 0.45,
          }}
        />
        <Button
          type="button"
          data-testid="boom-record"
          variant="ghost"
          size="icon"
          disabled={!session}
          onClick={recording ? undefined : onRecord}
          aria-label={recording ? 'Recording' : 'Start recording'}
          className={cn(
            'relative rounded-full bg-gradient-to-br from-[#ffcb6b] to-[#d9861c] font-semibold text-[#241a08] hover:bg-transparent',
            fullscreen ? 'size-28 text-xl' : 'size-24 text-lg',
          )}
        >
          {recording ? (
            <span data-testid="boom-countdown" className="tabular-nums">
              {secondsLeft}s
            </span>
          ) : (
            <span aria-hidden="true" className="relative block h-8 w-7">
              <span className="absolute left-1/2 top-0 h-4 w-3 -translate-x-1/2 rounded-full bg-[#241a08]" />
              <span className="absolute left-0 top-1.5 h-3.5 w-7 rounded-b-[12px] border-2 border-t-0 border-[#241a08]" />
            </span>
          )}
        </Button>
      </div>

      {recording && (
        // Fullscreen puts the two decisions at the bottom of the page as big
        // blocks, the way a game shows its answers; the compact control keeps
        // them as a pair of round buttons under the wave.
        <div
          className={cn(
            fullscreen
              ? 'fixed inset-x-0 bottom-0 z-10 mx-auto flex w-full max-w-3xl gap-3 px-4 pb-[max(1rem,env(safe-area-inset-bottom))] md:gap-4'
              : 'flex items-center gap-4',
          )}
        >
          <Button
            type="button"
            variant="destructive"
            data-testid="cancel-audio"
            aria-label="Cancel recording"
            onClick={onCancel}
            className={cn(
              'flex-col',
              fullscreen
                ? 'h-24 flex-1 gap-2 rounded-2xl text-sm font-semibold active:translate-y-0.5 md:h-32 md:text-base'
                : 'size-16 rounded-full',
            )}
          >
            <svg viewBox="0 0 24 24" className={fullscreen ? 'size-9' : 'size-7'} fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
              <path d="M6 6l12 12M18 6L6 18" />
            </svg>
            {fullscreen && 'Discard'}
          </Button>
          <Button
            type="button"
            data-testid="send-audio"
            aria-label="Send recording"
            onClick={onSend}
            className={cn(
              'flex-col',
              fullscreen
                ? 'h-24 flex-1 gap-2 rounded-2xl text-sm font-semibold active:translate-y-0.5 md:h-32 md:text-base'
                : 'size-16 rounded-full',
            )}
          >
            <svg viewBox="0 0 24 24" className={fullscreen ? 'size-9' : 'size-7'} fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M12 19V5M5 12l7-7 7 7" />
            </svg>
            {fullscreen && 'Send'}
          </Button>
        </div>
      )}

      {/* Fullscreen carries the same instruction on the blocks themselves, so
          the line would only repeat them. */}
      {!fullscreen && (
        <p className="text-xs text-muted-foreground">
          {recording ? 'Listening… tap send when you are done.' : 'Tap to blurt.'}
        </p>
      )}
    </div>
  )
}
