import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'

interface ComposerProps {
  disabled: boolean
  // focus moves the caret here, which is where cancelling a capture lands.
  focus: boolean
  onSend: (text: string) => Promise<boolean>
}

// TextComposer is the typed half of the one ingress, and the fallback whenever
// the microphone is unusable.
export function TextComposer({ disabled, focus, onSend }: ComposerProps) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const ref = useRef<HTMLTextAreaElement | null>(null)

  useEffect(() => {
    if (focus) ref.current?.focus()
  }, [focus])

  async function submit(event: FormEvent) {
    event.preventDefault()
    const value = text.trim()
    if (!value || busy) return
    setBusy(true)
    // The draft only leaves the box once the server has the Job.
    if (await onSend(value)) setText('')
    setBusy(false)
  }

  return (
    <form onSubmit={(event) => void submit(event)} className="flex flex-col gap-3 rounded-xl border border-border bg-card/80 p-3">
      <Textarea
        ref={ref}
        data-testid="capture-text"
        value={text}
        onChange={(event) => setText(event.target.value)}
        placeholder={disabled ? 'Finish the clip first…' : 'Or type it…'}
        rows={2}
        aria-label="Blurt text input"
      />
      <div className="flex justify-end">
        <Button type="submit" data-testid="send-text" disabled={disabled || busy || !text.trim()}>
          {busy ? 'Sending…' : 'Send'}
        </Button>
      </div>
    </form>
  )
}