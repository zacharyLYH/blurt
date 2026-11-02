// Traces API: what each stage did for one Reminder, oldest first. Debug only — no
// product surface reads this.
import { api } from './api'

// One Trace row. Detail is the stage's own payload, so it stays an open object: the
// waterfall shows it as it was written and adds no second schema to keep in step.
export interface Trace {
  traceId: number
  jobId: number
  stage: string
  timestamp: string
  detail: Record<string, unknown>
}

// A sidebar row: the reminder and how much history it has.
export interface TracedReminder {
  reminderId: number
  text: string
  timezone: string
  status: string
  lastAt: string
  traces: number
}

export interface TraceHistory {
  reminder: TracedReminder
  traces: Trace[]
}

export function listTracedReminders(): Promise<TracedReminder[]> {
  return api<{ reminders: TracedReminder[] }>('/api/traces').then((reply) => reply.reminders ?? [])
}

// A reminder that is not the caller's is a 404, the same answer as one that never
// existed — the page has nothing to show either way.
export function getTraceHistory(reminderId: number): Promise<TraceHistory> {
  return api<TraceHistory>(`/api/traces/${reminderId}`)
}
