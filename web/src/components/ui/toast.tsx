import * as ToastPrimitive from '@radix-ui/react-toast'
import { useSyncExternalStore } from 'react'
import { dismissToast, subscribeToasts, toastMessages } from '@/lib/toast'

// Toaster is mounted once, by App: the viewport is a fixed layer, so where it
// sits in the tree does not matter. The queue itself lives in lib/toast.ts.
export function Toaster() {
  const shown = useSyncExternalStore(subscribeToasts, toastMessages)

  return (
    <ToastPrimitive.Provider swipeDirection="down" duration={3200}>
      {shown.map((message) => (
        <ToastPrimitive.Root
          key={message.id}
          data-testid="toast"
          onOpenChange={(open) => {
            if (!open) dismissToast(message.id)
          }}
          className="blurt-toast pointer-events-auto flex w-full max-w-sm flex-col gap-1 rounded-xl border border-border bg-card px-4 py-3 shadow-lg"
        >
          <ToastPrimitive.Title className="text-sm font-medium">{message.title}</ToastPrimitive.Title>
          {message.description && (
            <ToastPrimitive.Description className="text-xs leading-relaxed text-muted-foreground">
              {message.description}
            </ToastPrimitive.Description>
          )}
        </ToastPrimitive.Root>
      ))}
      <ToastPrimitive.Viewport className="pointer-events-none fixed inset-x-0 bottom-24 z-[60] flex flex-col items-center gap-2 px-4" />
    </ToastPrimitive.Provider>
  )
}
