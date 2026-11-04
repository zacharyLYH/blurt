import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { ApiError, api } from './api'

export interface User {
  userId: number
  name: string
  email: string
  status: 'pending' | 'active'
  lastQuestionnaireVersion: number
  // owner is the one address the deployment names: it approves everyone else.
  owner: boolean
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
    void (async () => {
      // A PWA can be woken from a notification before the network is ready. A
      // failed request is not a signed-out user: only a 401 is. Retrying the
      // transient answers is what stops a cold start from bouncing to login.
      for (let attempt = 0; ; attempt++) {
        try {
          const reply = await api<UserEnvelope>('/api/user')
          if (live) setUser(reply.user)
          break
        } catch (err) {
          if (!live) return
          // A missing session is expected on first visit.
          if (err instanceof ApiError && err.status === 401) {
            setUser(null)
            break
          }
          if (attempt >= 2) {
            setUser(null)
            console.warn('Blurt: could not load user', err)
            break
          }
          await new Promise((resolve) => window.setTimeout(resolve, 400 * (attempt + 1)))
          if (!live) return
        }
      }
      if (live) setLoading(false)
    })()
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
