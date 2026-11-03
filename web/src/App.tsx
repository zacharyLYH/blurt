import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import Admin from './pages/Admin'
import Calendar from './pages/Calendar'
import Job from './pages/Job'
import JobReply from './pages/JobReply'
import Landing from './pages/Landing'
import Login from './pages/Login'
import NotificationDetail from './pages/NotificationDetail'
import Permissions from './pages/Permissions'
import Questionnaire from './pages/Questionnaire'
import questionnaire from './data/questionnaire/v1.json'
import Traces from './pages/Traces'
import { Toaster } from './components/ui/toast'
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
    // Keep the query too: a deep link can carry the month or any other state the
    // destination page needs (the calendar's ?month is the one that matters).
    return <Navigate to="/permissions" replace state={{ from: location.pathname + location.search }} />
  }
  return <>{children}</>
}

// Fresh checks the questionnaire version on every inside route, not just Home.
// Direct links and notification taps therefore cannot skip setup.
function Fresh({ children }: { children: React.ReactNode }) {
  const { user } = useUser()
  if (user && user.lastQuestionnaireVersion < questionnaire.version) {
    return <Navigate to="/questionnaire" replace />
  }
  return <>{children}</>
}

// Already current: setup is done, so the form forwards to Home instead of
// asking the same questions again. Corrections happen through a blurt.
function QuestionnaireEntry() {
  const { user } = useUser()
  if (user && user.lastQuestionnaireVersion >= questionnaire.version) {
    return <Navigate to="/home" replace />
  }
  return <Questionnaire />
}

// OwnerOnly is the portal's gate. The server is the real check — it answers 403
// to anyone else — so this only keeps a non-owner off a page that would fail.
function OwnerOnly({ children }: { children: React.ReactNode }) {
  const { user } = useUser()
  if (!user?.owner) return <Navigate to="/home" replace />
  return <>{children}</>
}

// CalendarAlias keeps the month someone already has in a link or a bookmark:
// the calendar is Home now, so the old path forwards with its query intact.
function CalendarAlias() {
  const { search } = useLocation()
  return <Navigate to={{ pathname: '/home', search }} replace />
}

export default function App() {
  return (
    <UserProvider>
      <BrowserRouter>
        <Routes>
          {/* The one public page. It never redirects a signed-in visitor: a
              marketing link should stay a marketing link. */}
          <Route path="/" element={<Landing />} />
          <Route path="/login" element={<Login />} />
          <Route
            path="/admin"
            element={
              <Protected>
                <OwnerOnly>
                  <Admin />
                </OwnerOnly>
              </Protected>
            }
          />
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
                  <QuestionnaireEntry />
                </Gated>
              </Protected>
            }
          />
          <Route
            path="/home"
            element={
              <Protected>
                <Gated>
                  <Fresh>
                    <Calendar />
                  </Fresh>
                </Gated>
              </Protected>
            }
          />
          <Route
            path="/jobs/:jobId"
            element={
              <Protected>
                <Gated>
                  <Fresh>
                    <Job />
                  </Fresh>
                </Gated>
              </Protected>
            }
          />
          {/* The boom for one Job's spoken reply. A route of its own, so the
              browser's back gesture is the way out of it. */}
          <Route
            path="/jobs/:jobId/reply"
            element={
              <Protected>
                <Gated>
                  <Fresh>
                    <JobReply />
                  </Fresh>
                </Gated>
              </Protected>
            }
          />
          {/* The calendar is Home now; the old path still works, month and all. */}
          <Route path="/calendar" element={<CalendarAlias />} />
          {/* The page a push opens: the Reminder itself, with feedback per submission. */}
          <Route
            path="/notifications/:reminderId"
            element={
              <Protected>
                <Gated>
                  <Fresh>
                    <NotificationDetail />
                  </Fresh>
                </Gated>
              </Protected>
            }
          />
          {/* The debug page: one reminder's stage history, indented by job. It is
              part of the owner's tooling, alongside the access portal. */}
          <Route
            path="/traces"
            element={
              <Protected>
                <OwnerOnly>
                  <Traces />
                </OwnerOnly>
              </Protected>
            }
          />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
        <Toaster />
      </BrowserRouter>
    </UserProvider>
  )
}
