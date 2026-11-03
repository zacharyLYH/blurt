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

// A turn is a turn: a clip and a typed line post to the same endpoint, and
// opening a conversation differs from continuing one only in the URL.
function postAudio(url: string, recording: Recording): Promise<Job> {
  return api<JobEnvelope>(url, {
    method: 'POST',
    headers: {
      'Content-Type': recording.blob.type,
      'X-Blurt-Duration-Ms': String(Math.round(recording.durationMs)),
      'X-Blurt-Timezone': deviceTimezone(),
    },
    body: recording.blob,
  }).then((reply) => reply.job)
}

function postText(url: string, text: string): Promise<Job> {
  return api<JobEnvelope>(url, {
    method: 'POST',
    headers: { 'X-Blurt-Timezone': deviceTimezone() },
    body: JSON.stringify({ text }),
  }).then((reply) => reply.job)
}

export function sendAudio(recording: Recording): Promise<Job> {
  return postAudio('/api/jobs', recording)
}

export function sendText(text: string): Promise<Job> {
  return postText('/api/jobs', text)
}

// The clarification reply is one more turn in the same conversation: the Job goes
// back to the queue, so the thread keeps polling and the agent picks the answer up.
export function replyToJob(jobId: number, text: string): Promise<Job> {
  return postText(`/api/jobs/${jobId}/messages`, text)
}

// replyWithAudio is the same turn spoken instead of typed: the capture's own
// upload, pointed at the conversation that already exists.
export function replyWithAudio(jobId: number, recording: Recording): Promise<Job> {
  return postAudio(`/api/jobs/${jobId}/messages`, recording)
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
