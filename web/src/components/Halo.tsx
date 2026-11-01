import { useRef, useState } from 'react'
import type { PointerEvent } from 'react'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

interface HaloProps {
  onSpawn: () => void
  disabled?: boolean
}

const SIZE = 64
// Anything shorter than this is a tap: a finger never sits perfectly still, and
// a nudge must not turn into a recording.
const SLOP = 6

function clamp(x: number, y: number) {
  const edge = 8
  return {
    x: Math.min(Math.max(x, edge), window.innerWidth - SIZE - edge),
    y: Math.min(Math.max(y, edge), window.innerHeight - SIZE - edge),
  }
}

// Halo is the calendar's door to voice: a breathing disc that drags anywhere on
// the page and spawns the boom on a tap. Moving it is free — the travel is the
// drag — and only a tap (or Enter) starts a clip.
export function Halo({ onSpawn, disabled = false }: HaloProps) {
  const [at, setAt] = useState<{ x: number; y: number } | null>(null)
  // Where the finger went down, how far into the disc it landed, and whether it
  // has travelled far enough to be a drag rather than a tap.
  const grab = useRef<{ x: number; y: number; dx: number; dy: number; moved: boolean } | null>(null)
  // The release still fires a click: this keeps the tap path from spawning twice.
  const dragged = useRef(false)

  function down(event: PointerEvent<HTMLButtonElement>) {
    if (disabled) return
    const rect = event.currentTarget.getBoundingClientRect()
    grab.current = {
      x: event.clientX,
      y: event.clientY,
      dx: event.clientX - rect.left,
      dy: event.clientY - rect.top,
      moved: false,
    }
    event.currentTarget.setPointerCapture(event.pointerId)
  }

  function move(event: PointerEvent<HTMLButtonElement>) {
    const held = grab.current
    if (!held) return
    if (Math.hypot(event.clientX - held.x, event.clientY - held.y) > SLOP) held.moved = true
    setAt(clamp(event.clientX - held.dx, event.clientY - held.dy))
  }

  function up() {
    dragged.current = grab.current?.moved ?? false
    grab.current = null
  }

  return (
    <Button
      type="button"
      data-testid="halo"
      aria-label="Record a blurt"
      title="Tap to talk. Drag me anywhere."
      disabled={disabled}
      variant="ghost"
      size="icon"
      onPointerDown={down}
      onPointerMove={move}
      onPointerUp={up}
      onPointerCancel={up}
      onClick={() => {
        if (dragged.current) {
          dragged.current = false
          return
        }
        onSpawn()
      }}
      style={at ? { left: at.x, top: at.y } : undefined}
      className={cn(
        'fixed z-30 size-16 touch-none bg-gradient-to-br from-[#ffcb6b] to-[#d9861c] text-[#241a08] shadow-[0_10px_40px_-8px_rgba(217,134,28,0.7)] select-none hover:bg-transparent active:scale-95',
        at ? '' : 'right-4 bottom-28',
      )}
    >
      <span
        aria-hidden="true"
        className="voice-cue-pulse absolute inset-0 rounded-full border-2 border-primary/40"
      />
      <span
        aria-hidden="true"
        className="voice-cue-pulse absolute -inset-4 rounded-full border border-primary/25"
        style={{ animationDelay: '-1.2s' }}
      />
      <span aria-hidden="true" className="relative block h-7 w-6">
        <span className="absolute left-1/2 top-0 h-4 w-3 -translate-x-1/2 rounded-full bg-[#241a08]" />
        <span className="absolute left-0 top-1.5 h-4 w-6 rounded-b-[11px] border-[3px] border-t-0 border-[#241a08]" />
      </span>
    </Button>
  )
}
