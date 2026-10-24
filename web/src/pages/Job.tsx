import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { JobThread } from '@/components/JobThread'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { ApiError } from '@/lib/api'
import { getJob, type Job as JobRecord } from '@/lib/jobs'

// One capture, by id. A notification links straight here, so the page has to
// survive a cold start on its own: it loads the Job and hands it to the same
// thread the capture screen renders, which keeps polling until it settles.
export default function Job() {
  const navigate = useNavigate()
  const { jobId } = useParams()
  const id = Number(jobId)
  const valid = Number.isInteger(id) && id > 0
  const [job, setJob] = useState<JobRecord | null>(null)
  const [failure, setFailure] = useState<string | null>(null)

  useEffect(() => {
    if (!valid) return
    let live = true
    getJob(id)
      .then((next) => {
        if (live) setJob(next)
      })
      .catch((err: unknown) => {
        if (!live) return
        setFailure(err instanceof ApiError && err.status === 404 ? 'That capture no longer exists.' : 'Could not load that capture.')
      })
    return () => {
      live = false
    }
  }, [id, valid])

  // A malformed id never reaches the network, so its message is derived rather
  // than state.
  const error = valid ? failure : 'That capture no longer exists.'

  return (
    <main className="grid min-h-dvh place-items-center px-4 py-8">
      <Card className="w-full max-w-xl gap-3 bg-card/80 p-5">
        {error && (
          <Alert variant="destructive" data-testid="job-error">
            <AlertDescription className="text-destructive">{error}</AlertDescription>
          </Alert>
        )}
        {!job && !error && (
          <CardContent className="gap-3">
            <Skeleton className="h-4 w-2/3" />
            <Skeleton className="h-4 w-1/2" />
          </CardContent>
        )}
        {job && <JobThread job={job} onReset={() => navigate('/home')} />}
      </Card>
    </main>
  )
}