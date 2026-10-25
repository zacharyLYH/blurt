// Job capture API: one binary blob in, a pollable conversation out.
import { api } from './api'
import type { Recording } from './recorder'

// A Job's server Status: running, or one of the three ways it stops.
export type JobStatus = 'open' | 'clarifying' | 'complete' | 'failed'

export interface JobMessage {
  role: 'user' | 'assistant'
  text?: string
  audio?: string
  at: string
}

export interface Job {
  jobId: number
  status: JobStatus
  conversation: JobMessage[]
  createdAt: string
}

interface JobEnvelope {
  job: Job
}

// A sidebar row: what the list renders, not the whole thread. The server sends
// the preview, so polling the sidebar never pulls a full transcript.
export interface JobSummary {
  jobId: number
  status: JobStatus
  preview: string
  createdAt: string
}

export function sendAudio(recording: Recording): Promise<Job> {
  return api<JobEnvelope>('/api/jobs', {
    method: 'POST',
    headers: {
      'Content-Type': recording.blob.type,
      'X-Blurt-Duration-Ms': String(Math.round(recording.durationMs)),
      'X-Blurt-Timezone': deviceTimezone(),
    },
    body: recording.blob,
  }).then((reply) => reply.job)
}

export function sendText(text: string): Promise<Job> {
  return api<JobEnvelope>('/api/jobs', {
    method: 'POST',
    headers: { 'X-Blurt-Timezone': deviceTimezone() },
    body: JSON.stringify({ text }),
  }).then((reply) => reply.job)
}

// The clarification reply is one more turn in the same conversation: the Job goes
// back to the queue, so the thread keeps polling and the agent picks the answer up.
export function replyToJob(jobId: number, text: string): Promise<Job> {
  return api<JobEnvelope>(`/api/jobs/${jobId}/messages`, {
    method: 'POST',
    headers: { 'X-Blurt-Timezone': deviceTimezone() },
    body: JSON.stringify({ text }),
  }).then((reply) => reply.job)
}

// replyWithAudio is the same turn spoken instead of typed. It is the capture's
// own upload to a different URL, because the server takes one ingress for both:
// a turn is a turn, whether it opened a conversation or continued one.
export function replyWithAudio(jobId: number, recording: Recording): Promise<Job> {
  return api<JobEnvelope>(`/api/jobs/${jobId}/messages`, {
    method: 'POST',
    headers: {
      'Content-Type': recording.blob.type,
      'X-Blurt-Duration-Ms': String(Math.round(recording.durationMs)),
      'X-Blurt-Timezone': deviceTimezone(),
    },
    body: recording.blob,
  }).then((reply) => reply.job)
}

// deviceTimezone is the anchor the agent resolves a relative time against. A
// browser that cannot report one sends nothing, and the agent asks instead of
// guessing.
function deviceTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone
  } catch {
    return ''
  }
}

export function getJob(jobId: number): Promise<Job> {
  return api<JobEnvelope>(`/api/jobs/${jobId}`).then((reply) => reply.job)
}

// The open-conversation sidebar: one row per Job the agent has not closed yet.
export function listJobs(): Promise<JobSummary[]> {
  return api<{ jobs: JobSummary[] }>('/api/jobs').then((reply) => reply.jobs)
}

// A Job is settled for the UI once the agent has stopped on it: it closed the
// capture, failed, or asked the user something. A Job still in a pipeline status is
// the agent's, so the thread keeps polling — which is also how a reply resumes it,
// since the server hands the Job back to a queue.
export function isSettled(job: Job): boolean {
  return job.status === 'failed' || job.status === 'complete' || job.status === 'clarifying'
}
