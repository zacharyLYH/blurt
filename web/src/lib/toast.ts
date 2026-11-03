// The toast queue, in the module rather than a context: any caller can say
// `toast(...)` without threading a provider through the tree, and there is only
// ever one viewport. The component that renders it lives in
// components/ui/toast.tsx, which is why this is a separate file — a module that
// exports both would break fast refresh.
export interface ToastMessage {
  id: number
  title: string
  description?: string
}

let nextId = 1
let messages: ToastMessage[] = []
const listeners = new Set<() => void>()

function publish() {
  for (const listener of listeners) listener()
}

// toast is a receipt, not a decision: it says the thing the user just did
// worked, and it goes away by itself. Anything that needs an answer is a dialog.
export function toast(message: Omit<ToastMessage, 'id'>): void {
  messages = [...messages, { ...message, id: nextId++ }]
  publish()
}

export function dismissToast(id: number): void {
  messages = messages.filter((message) => message.id !== id)
  publish()
}

export function subscribeToasts(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export function toastMessages(): ToastMessage[] {
  return messages
}
