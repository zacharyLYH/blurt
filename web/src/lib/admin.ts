// Account management is the owner's portal: who applied, and the yes or no that
// decides it. Approving mails the applicant the way in; rejecting deletes them.
import { api } from './api'
import type { User } from './user'

export interface Applicant {
  userId: number
  name: string
  email: string
  status: 'pending' | 'active'
}

export async function listApplicants(): Promise<Applicant[]> {
  const reply = await api<{ users: Applicant[] }>('/api/admin/users')
  return reply.users
}

export async function approveApplicant(userId: number): Promise<User> {
  const reply = await api<{ user: User }>(`/api/admin/users/${userId}/approve`, { method: 'POST' })
  return reply.user
}

export async function rejectApplicant(userId: number): Promise<void> {
  await api(`/api/admin/users/${userId}/reject`, { method: 'POST' })
}