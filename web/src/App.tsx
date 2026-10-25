import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import Home from './pages/Home'
import Job from './pages/Job'
import JobReply from './pages/JobReply'
import Login from './pages/Login'
import Permissions from './pages/Permissions'
import Questionnaire from './pages/Questionnaire'
import { permissionsChecked } from './lib/permissions'
import { UserProvider, useUser } from './lib/user'

function Protected({ children }: { children: React.ReactNode }) {
  const { user, loading } = useUser()
  if (loading) {
    return <main className="grid min-h-dvh place-items-center text-sm text-muted-foreground">Loading…</main>
  }
  if (!user) return <Navigate to="/login" replace />
  return <>{children}</>
}

// Gated runs the permission check once per app entry — the flag lives in the
// module, so a reload or a later visit asks again — and it runs ahead of the
// questionnaire, because setup is where the app explains what it needs.
function Gated({ children }: { children: React.ReactNode }) {
  const location = useLocation()
  // A notification can deep-link into the app, so the gate remembers where the
  // user was headed instead of dropping them on Home.
  if (!permissionsChecked()) {
    return <Navigate to="/permissions" replace state={{ from: location.pathname }} />
  }
  return <>{children}</>
}

export default function App() {
  return (
    <UserProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route
            path="/permissions"
            element={
              <Protected>
                <Permissions />
              </Protected>
            }
          />
          <Route
            path="/questionnaire"
            element={
              <Protected>
                <Gated>
                  <Questionnaire />
                </Gated>
              </Protected>
            }
          />
          <Route
            path="/home"
            element={
              <Protected>
                <Gated>
                  <Home />
                </Gated>
              </Protected>
            }
          />
          <Route path="/jobs/:jobId" element={<Protected><Gated><Job /></Gated></Protected>} />
          {/* The boom for one Job's spoken reply. A route of its own, so the
              browser's back gesture is the way out of it. */}
          <Route path="/jobs/:jobId/reply" element={<Protected><Gated><JobReply /></Gated></Protected>} />
          <Route path="*" element={<Navigate to="/home" replace />} />
        </Routes>
      </BrowserRouter>
    </UserProvider>
  )
}
