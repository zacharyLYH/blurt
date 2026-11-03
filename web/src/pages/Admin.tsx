import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { buttonVariants } from '@/components/ui/button-variants'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { Skeleton } from '@/components/ui/skeleton'
import { approveApplicant, listApplicants, rejectApplicant, type Applicant } from '@/lib/admin'
import { useUser } from '@/lib/user'
import { messageOf } from '@/lib/utils'

// The owner's portal: who is waiting, and the one decision each row needs.
// Everything else about an account is the app's business, not the owner's.
export default function Admin() {
  const { user } = useUser()
  const [users, setUsers] = useState<Applicant[] | null>(null)
  const [busy, setBusy] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(() => {
    listApplicants()
      .then(setUsers)
      .catch((err: unknown) => setError(messageOf(err)))
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  async function decide(applicant: Applicant, verdict: 'approve' | 'reject') {
    setBusy(applicant.userId)
    setError(null)
    try {
      await (verdict === 'approve' ? approveApplicant : rejectApplicant)(applicant.userId)
      refresh()
    } catch (err) {
      setError(messageOf(err))
    } finally {
      setBusy(null)
    }
  }

  const pending = users?.filter((applicant) => applicant.status === 'pending') ?? []
  const active = users?.filter((applicant) => applicant.status === 'active') ?? []

  return (
    <main className="mx-auto flex min-h-dvh w-full max-w-2xl flex-col gap-6 px-4 py-8 sm:px-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-1">
          <h1 className="text-2xl font-semibold tracking-tight">Access requests</h1>
          <p className="max-w-sm text-sm text-muted-foreground">
            Approving emails the applicant the way in. Rejecting removes the request.
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Link to="/traces" className={buttonVariants({ variant: 'ghost', size: 'sm' })}>
            Traces
          </Link>
          <Link to="/home" className={buttonVariants({ variant: 'ghost', size: 'sm' })}>
            Back to Blurt
          </Link>
        </div>
      </div>

      {error && (
        <Alert variant="destructive">
          <AlertDescription className="break-words text-destructive">{error}</AlertDescription>
        </Alert>
      )}

      <Card className="gap-4 bg-card/80" data-testid="admin-queue">
        <CardHeader className="gap-1">
          <CardTitle className="flex items-center gap-2 text-base">
            Waiting
            <Badge variant="outline" className="border-amber-500/30 text-amber-300" data-testid="admin-pending-count">
              {users ? pending.length : '…'}
            </Badge>
          </CardTitle>
          <CardDescription className="text-xs">
            Oldest first. Only an approved address can ask Blurt for a PIN.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {users === null ? (
            <div className="flex flex-col gap-2">
              <Skeleton className="h-16 w-full" />
              <Skeleton className="h-16 w-full" />
            </div>
          ) : pending.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground" data-testid="admin-empty">
              Nothing waiting. Every application has been answered.
            </p>
          ) : (
            <ItemGroup className="gap-2" data-testid="admin-pending">
              {pending.map((applicant) => (
                <Item
                  key={applicant.userId}
                  data-testid="admin-user"
                  variant="outline"
                  size="sm"
                  className="items-start justify-between gap-3 p-3"
                >
                  <ItemContent>
                    <ItemTitle>{applicant.name}</ItemTitle>
                    <ItemDescription className="break-all text-xs">{applicant.email}</ItemDescription>
                  </ItemContent>
                  <ItemActions className="shrink-0">
                    <Button
                      size="sm"
                      data-testid="admin-approve"
                      disabled={busy === applicant.userId}
                      onClick={() => void decide(applicant, 'approve')}
                    >
                      Approve
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-muted-foreground hover:text-destructive"
                      data-testid="admin-reject"
                      disabled={busy === applicant.userId}
                      onClick={() => void decide(applicant, 'reject')}
                    >
                      Reject
                    </Button>
                  </ItemActions>
                </Item>
              ))}
            </ItemGroup>
          )}
        </CardContent>
      </Card>

      <Card className="gap-4 bg-card/80">
        <CardHeader className="gap-1">
          <CardTitle className="flex items-center gap-2 text-base">
            Approved
            <Badge variant="outline" className="border-emerald-500/30 text-emerald-300" data-testid="admin-approved-count">
              {users ? active.length : '…'}
            </Badge>
          </CardTitle>
        </CardHeader>
        <CardContent>
          {users !== null && active.length === 0 && (
            <p className="py-4 text-center text-sm text-muted-foreground">Nobody yet.</p>
          )}
          <ItemGroup className="gap-2" data-testid="admin-approved">
            {active.map((applicant) => (
              <Item key={applicant.userId} variant="muted" size="sm" className="justify-between gap-3 p-3">
                <ItemContent>
                  <ItemTitle>
                    {applicant.name}
                    {applicant.email === user?.email && (
                      <Badge variant="outline" className="text-[10px]">
                        you
                      </Badge>
                    )}
                  </ItemTitle>
                  <ItemDescription className="break-all text-xs">{applicant.email}</ItemDescription>
                </ItemContent>
                <Badge variant="outline" className="border-emerald-500/30 text-emerald-300">
                  Active
                </Badge>
              </Item>
            ))}
          </ItemGroup>
        </CardContent>
      </Card>
    </main>
  )
}