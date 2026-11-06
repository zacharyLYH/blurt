import { useState } from 'react'
import type { FormEvent } from 'react'
import { PullUpMic } from '@/components/PullUpMic'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'

interface ComposerProps {
  disabled?: boolean
  onSend: (text: string) => Promise<boolean>
  // The voice door is optional: a screen with its own recorder (the calendar's
  // halo) hides this one instead of offering two ways into the same clip.
  onVoice?: () => void
  voiceDisabled?: boolean
  // A capture and a reply read differently, so the test handles and the
  // placeholder have to be the caller's.
  textTestId?: string
  sendTestId?: string
  placeholder?: string
}

// TextComposer is the one ingress: type here, or pull the mic up to talk. It is
// the chat's bottom bar, so it stays a single row until the text wraps.
export function TextComposer({
  disabled = false,
  onSend,
  onVoice,
  voiceDisabled = false,
  textTestId = 'capture-text',
  sendTestId = 'send-text',
  placeholder = 'Message Blurt…',
}: ComposerProps) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)

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
    <form
      onSubmit={(event) => void submit(event)}
      className="flex items-end gap-1.5 rounded-3xl border border-border bg-card/80 p-1.5 pl-4 shadow-lg"
    >
      <Textarea
        data-testid={textTestId}
        value={text}
        onChange={(event) => setText(event.target.value)}
        placeholder={disabled ? 'Wait for Blurt…' : placeholder}
        rows={1}
        aria-label="Blurt text input"
        className="field-sizing-content max-h-40 min-h-10 resize-none border-0 bg-transparent px-0 py-2 text-base shadow-none focus-visible:border-0 focus-visible:ring-0 md:text-base"
      />
      {onVoice && <PullUpMic onConfirm={onVoice} disabled={voiceDisabled} />}
      <Button
        type="submit"
        size="icon"
        data-testid={sendTestId}
        disabled={disabled || busy || !text.trim()}
        aria-label="Send"
        className="size-10 shrink-0"
      >
        <svg viewBox="0 0 24 24" className="size-5" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M12 19V5M5 12l7-7 7 7" />
        </svg>
      </Button>
    </form>
  )
}
