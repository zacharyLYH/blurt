// Reminders API: what is scheduled, and what already happened.
import { api } from './api'

// A Reminder's lifecycle. It starts resolved, a one-off becomes fired when the emitter
// sends it, and cancelling is a status too — the row is kept, so the history stays
// readable instead of the Reminder simply vanishing.
export type ReminderStatus = 'resolved' | 'fired' | 'cancelled'

export type ReminderFrequency = 'daily' | 'weekly' | 'monthly' | 'yearly'

// A repeat's pattern. Only the calendar parts its frequency needs are set, and the
// weekday is Go's own numbering, where 0 is Sunday.
export interface ReminderRule {
  frequency: ReminderFrequency
  time: string
  weekday?: number
  day?: number
  month?: number
}

export interface Reminder {
  reminderId: number
  text: string
  timezone: string
  localDateTime?: string
  emitAtUtc?: string
  rule?: ReminderRule
  nextEmitAtUtc?: string
  status: ReminderStatus
  sourceJobId?: number
  createdAt: string
}

// The page's two lists, split by status on the server: a repeat stays resolved between
// firings, so it is never in the past until it is called off.
export interface ReminderLists {
  upcoming: Reminder[]
  past: Reminder[]
}

export function listReminders(): Promise<ReminderLists> {
  return api<ReminderLists>('/api/reminders')
}

// cancelReminder calls one off. The row stays, so the past list keeps its history.
export function cancelReminder(reminderId: number): Promise<void> {
  return api<void>(`/api/reminders/${reminderId}/cancel`, { method: 'POST' })
}