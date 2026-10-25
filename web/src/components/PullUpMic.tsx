import { useCallback, useRef, useState } from 'react'
import type { PointerEvent } from 'react'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

interface PullUpMicProps {
  // onConfirm fires once the gesture or the tap has committed.
  onConfirm: () => void
  disabled?: boolean
}

// thresholdPx is the travel that commits the gesture: a third of the viewport,
// measured from where the drag started. Long enough that a scroll or a mis-tap
// cannot reach it, short enough to be one flick.
function thresholdPx(): number {
  return window.innerHeight / 3
}

// PullUpMic is the thread's invitation to answer out loud: a labelled halo that
// drags upward with the pointer and commits once it has travelled far enough.
// Pulling up reads as raising a microphone, and it is vertical, so it never
// competes with the browser's own back-swipe.
//
// Tapping commits too. A gesture can never be the only door — on a desktop, a
// keyboard or a screen reader there is no swipe to make, and this is the voice
// path in a voice-first app.
export function PullUpMic({ onConfirm, disabled = false }: PullUpMicProps) {
  const [travel, setTravel] = useState(0)
  const [dragging, setDragging] = useState(false)
  const startY = useRef(0)
  // A drag that commits still fires a click afterwards; this keeps the tap path
  // from navigating a second time.
  const justDragged = useRef(false)

  const armed = travel >= thresholdPx()

  const down = useCallback(
    (event: PointerEvent<HTMLButtonElement>) => {
      if (disabled) return
      event.currentTarget.setPointerCapture(event.pointerId)
      startY.current = event.clientY
      justDragged.current = false
      setDragging(true)
    },
    [disabled],
  )

  const move = useCallback(
    (event: PointerEvent<HTMLButtonElement>) => {
      if (!dragging) return
      const raw = Math.max(0, startY.current - event.clientY)
      // Past the threshold the halo slows down: the travel has already
      // committed, and the resistance is how the gesture says so.
      const limit = thresholdPx()
      setTravel(raw > limit ? limit + (raw - limit) * 0.35 : raw)
    },
    [dragging],
  )

  const up = useCallback(() => {
    if (!dragging) return
    setDragging(false)
    if (travel >= thresholdPx()) {
      justDragged.current = true
      onConfirm()
      return
    }
    setTravel(0)
  }, [dragging, travel, onConfirm])

  const click = useCallback(() => {
    if (justDragged.current) {
      justDragged.current = false
      return
    }
    if (!disabled) onConfirm()
  }, [disabled, onConfirm])

  return (
    <div className="flex flex-col items-center gap-1.5 pb-1">
      <Button
        type="button"
        data-testid="pull-up-mic"
        data-armed={armed ? 'true' : 'false'}
        aria-label="Answer with your voice"
        disabled={disabled}
        variant="ghost"
        size="icon"
        onPointerDown={down}
        onPointerMove={move}
        onPointerUp={up}
        onPointerCancel={up}
        onClick={click}
        style={{
          // The drag must not scroll the thread underneath it.
          touchAction: 'none',
          transform: `translateY(-${travel}px)`,
          transition: dragging ? 'none' : 'transform 260ms cubic-bezier(0.34, 1.56, 0.64, 1)',
        }}
        className="relative size-16 hover:bg-primary/10"
      >
        <span
          aria-hidden="true"
          className={cn('absolute inset-0 rounded-full border border-primary/40', !dragging && 'voice-cue-pulse')}
        />
        <span
          aria-hidden="true"
          className={cn('absolute inset-2 rounded-full border transition-colors', armed ? 'border-primary/70' : 'border-primary/20')}
        />
        <span aria-hidden="true" className={cn('relative block h-7 w-6', !dragging && 'voice-cue-nudge')}>
          <span className="absolute left-1/2 top-0 h-3.5 w-2.5 -translate-x-1/2 rounded-full bg-primary" />
          <span className="absolute left-0 top-1 h-6 rounded-b-[11px] border-2 border-t-0 border-primary" />
        </span>
      </Button>
      <p data-testid="pull-up-hint" className="text-xs text-muted-foreground">
        {armed ? 'Release to answer' : dragging ? 'Keep going…' : 'Pull up to answer out loud'}
      </p>
    </div>
  )
}