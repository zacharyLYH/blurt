import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import type { MicSession } from '@/lib/recorder'

interface BoomProps {
  session: MicSession | null
  recording: boolean
  remainingMs: number
  onRecord: () => void
  onCancel: () => void
  onSend: () => void
}

// Boom is the capture control: a ring that breathes with the microphone and the
// two decisions a clip needs — keep it or throw it away.
export function Boom({ session, recording, remainingMs, onRecord, onCancel, onSend }: BoomProps) {
  const [level, setLevel] = useState(0)

  // Sampled per frame, and only while recording, so an idle screen costs
  // nothing. Rising fast and falling slowly is what reads as a breath rather
  // than a flicker.
  useEffect(() => {
    if (!recording || !session) return
    let frame = 0
    let smoothed = 0
    const tick = () => {
      const reading = session.loudness()
      smoothed = reading > smoothed ? reading : smoothed * 0.88
      setLevel(smoothed)
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [recording, session])

  const scale = recording ? 1 + level * 0.3 : 1
  const secondsLeft = Math.max(0, Math.ceil(remainingMs / 1000))

  return (
    <div className="flex flex-col items-center gap-5">
      <div data-testid="boom" className="relative grid size-52 place-items-center md:size-60">
        {[0, 1, 2].map((ring) => (
          <span
            key={ring}
            aria-hidden="true"
            className="absolute left-1/2 top-1/2 rounded-full border border-primary/25"
            style={{
              width: `${40 + ring * 30}%`,
              height: `${40 + ring * 30}%`,
              transform: `translate(-50%, -50%) scale(${scale})`,
            }}
          />
        ))}
        <Button
          type="button"
          data-testid="boom-record"
          variant="ghost"
          size="icon"
          disabled={!session}
          onClick={recording ? undefined : onRecord}
          aria-label={recording ? 'Recording' : 'Start recording'}
          className="relative size-24 bg-gradient-to-br from-[#ffcb6b] to-[#d9861c] text-lg font-semibold text-[#241a08] hover:bg-transparent"
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
        <div className="flex items-center gap-4">
          <Button
            type="button"
            variant="destructive"
            size="icon"
            className="size-16"
            data-testid="cancel-audio"
            aria-label="Cancel recording"
            onClick={onCancel}
          >
            <svg viewBox="0 0 24 24" className="size-7" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true">
              <path d="M6 6l12 12M18 6L6 18" />
            </svg>
          </Button>
          <Button
            type="button"
            size="icon"
            className="size-16"
            data-testid="send-audio"
            aria-label="Send recording"
            onClick={onSend}
          >
            <svg viewBox="0 0 24 24" className="size-7" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M12 19V5M5 12l7-7 7 7" />
            </svg>
          </Button>
        </div>
      )}

      <p className="text-xs text-muted-foreground">
        {recording ? 'Listening… tap send when you are done.' : 'Tap to blurt.'}
      </p>
    </div>
  )
}