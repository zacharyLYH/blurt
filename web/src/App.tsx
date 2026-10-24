import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import Home from './pages/Home'
import Login from './pages/Login'
import Questionnaire from './pages/Questionnaire'
import { UserProvider, useUser } from './lib/user'

function Protected({ children }: { children: React.ReactNode }) {
  const { user, loading } = useUser()
  if (loading) {
    return <main className="grid min-h-dvh place-items-center text-sm text-muted-foreground">Loading…</main>
  }
  if (!user) return <Navigate to="/login" replace />
  return <>{children}</>
}

export default function App() {
  return (
    <UserProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route
            path="/questionnaire"
            element={
              <Protected>
                <Questionnaire />
              </Protected>
            }
          />
          <Route
            path="/home"
            element={
              <Protected>
                <Home />
              </Protected>
            }
          />
          <Route path="*" element={<Navigate to="/home" replace />} />
        </Routes>
      </BrowserRouter>
    </UserProvider>
  )
}
