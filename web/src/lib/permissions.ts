// Microphone permission. The app asks as early as it can on every entry, because
// a permission can be revoked at any time in browser settings and the only way
// to notice is to ask again. Notification permission is push.ts' business, since
// granting it and minting a subscription are the same step.
//
// Asking is not the same as being able to prompt: browsers stop prompting once a
// user has denied a permission, so a denied state is surfaced (with what to do
// about it) rather than requested.
export type PermissionState = 'granted' | 'prompt' | 'denied' | 'unsupported'

// checked is deliberately module state, not storage: a reload, a new tab, or a
// later visit is a new entry, which is exactly when the app should ask again.
let checked = false

export function permissionsChecked(): boolean {
  return checked
}

export function markPermissionsChecked(): void {
  checked = true
}

// requestMicrophone both checks and asks: getUserMedia is the only call that
// raises the browser's microphone prompt, and on an already-granted permission
// it resolves without showing anything. The stream is released at once —
// recording belongs to the capture screen.
export async function requestMicrophone(): Promise<PermissionState> {
  if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia) return 'unsupported'
  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
    stream.getTracks().forEach((track) => track.stop())
    return 'granted'
  } catch (err) {
    if (err instanceof DOMException) {
      // No device at all is not a permission problem.
      if (err.name === 'NotFoundError' || err.name === 'OverconstrainedError') return 'unsupported'
      if (err.name === 'NotAllowedError' || err.name === 'SecurityError') return 'denied'
    }
    return 'denied'
  }
}