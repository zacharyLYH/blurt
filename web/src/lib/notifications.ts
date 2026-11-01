// Notifications API: the Reminder a poll found due, its feedback, and the calendar.
import { api } from './api'

// The closed set the "what went wrong?" form offers. The slug is what the server
// stores; the label is what the user reads.
export type FailureMode =
  | 'wrong_reminder'
  | 'wrong_date_time_zone'
  | 'wrong_or_irrelevant_context'
  | 'duplicate_too_many'
  | 'other'

export const failureModes: { value: FailureMode; label: string }[] = [
  { value: 'wrong_reminder', label: 'Wrong reminder' },
  { value: 'wrong_date_time_zone', label: 'Wrong date, time or zone' },
  { value: 'wrong_or_irrelevant_context', label: 'Wrong or irrelevant context' },
  { value: 'duplicate_too_many', label: 'Duplicate or too many' },
  { value: 'other', label: 'Something else' },
]

// A notification is the Reminder itself, read off the emitter's poll: its own words,
// zone, and the instant it is (next) about. Never a stored copy or a Trace row.
export interface Notification {
  reminderId: number
  text: string
  timezone: string
  at: string
}

export interface NotificationMonth {
  month: string
  timezone: string
  entries: Notification[]
}

export function getNotificationDetail(reminderId: number): Promise<Notification> {
  return api<Notification>(`/api/notifications/${reminderId}`)
}

// An empty comment is omitted rather than sent as an empty string. Every call makes
// a new report: a repeat collects one per firing.
export async function submitNotificationFeedback(
  reminderId: number,
  failureMode: FailureMode,
  comment: string,
): Promise<void> {
  await api(`/api/notifications/${reminderId}/feedback`, {
    method: 'POST',
    body: JSON.stringify({ failureMode, comment: comment.trim() || undefined }),
  })
}

// The month is the one the user is viewing, in the zone they are viewing it from.
// Nothing is cached: a month change asks the server again.
export function listNotificationsForMonth(month: string, timezone: string): Promise<NotificationMonth> {
  return api<NotificationMonth>(
    `/api/notifications?month=${encodeURIComponent(month)}&tz=${encodeURIComponent(timezone)}`,
  )
}
