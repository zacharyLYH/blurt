// Job capture API: one binary blob in, a pollable conversation out.
import { api } from './api'
import type { Recording } from './recorder'

export type JobStatus = 'queued' | 'processing' | 'clarifying' | 'complete' | 'failed'

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
    },
    body: recording.blob,
  }).then((reply) => reply.job)
}

export function sendText(text: string): Promise<Job> {
  return api<JobEnvelope>('/api/jobs', {
    method: 'POST',
    body: JSON.stringify({ text }),
  }).then((reply) => reply.job)
}

export function getJob(jobId: number): Promise<Job> {
  return api<JobEnvelope>(`/api/jobs/${jobId}`).then((reply) => reply.job)
}

// The open-conversation sidebar: one row per Job the agent has not closed yet.
export function listJobs(): Promise<JobSummary[]> {
  return api<{ jobs: JobSummary[] }>('/api/jobs').then((reply) => reply.jobs)
}

// A Job is settled for the UI once the transcript is in, or once it has failed.
// A transcribed Job stays queued for the agent, so status alone is not enough.
export function isSettled(job: Job): boolean {
  if (job.status === 'failed' || job.status === 'complete' || job.status === 'clarifying') return true
  return job.conversation.some((message) => message.role === 'user' && Boolean(message.text))
}
