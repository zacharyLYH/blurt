import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { ApiError, api } from './api'

export interface User {
  userId: number
  name: string
  email: string
  lastQuestionnaireVersion: number
}

export interface UserEnvelope {
  user: User
}

interface UserState {
  user: User | null
  loading: boolean
  setUser: (user: User | null) => void
}

const UserContext = createContext<UserState | null>(null)

// Identity comes from the HttpOnly session cookie; the client never stores
// a token. A 401 simply means "not signed in".
export function UserProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let live = true
    api<UserEnvelope>('/api/user')
      .then((reply) => {
        if (live) setUser(reply.user)
      })
      .catch((err) => {
        if (!live) return
        setUser(null)
        // A missing session is expected on first visit.
        if (!(err instanceof ApiError && err.status === 401)) console.warn('Blurt: could not load user', err)
      })
      .finally(() => {
        if (live) setLoading(false)
      })
    return () => {
      live = false
    }
  }, [])

  return <UserContext.Provider value={{ user, loading, setUser }}>{children}</UserContext.Provider>
}

export function useUser(): UserState {
  const state = useContext(UserContext)
  if (!state) throw new Error('useUser must be used inside UserProvider')
  return state
}
