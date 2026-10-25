import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// messageOf is the one way a caught error becomes something to show a user: an
// Error carries its own sentence, anything else is unexplained.
export function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : 'Something went wrong.'
}
