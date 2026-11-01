import { Boom } from '@/components/Boom'
import type { MicSession } from '@/lib/recorder'

interface VoiceScreenProps {
  session: MicSession | null
  remainingMs: number
  onRecord: () => void
  onCancel: () => void
  onSend: () => void
}

// VoiceScreen is the voice-first page: no chrome, no typing, just the breathing
// recorder and the two things a clip can do.
export function VoiceScreen({ session, remainingMs, onRecord, onCancel, onSend }: VoiceScreenProps) {
  return (
    <div
      data-testid="voice-screen"
      className="fixed inset-0 z-50 flex flex-col items-center justify-center bg-background px-6"
    >
      <Boom
        fullscreen
        session={session}
        recording
        remainingMs={remainingMs}
        onRecord={onRecord}
        onCancel={onCancel}
        onSend={onSend}
      />
    </div>
  )
}
