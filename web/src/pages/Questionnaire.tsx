import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import questionnaire from '@/data/questionnaire/v1.json'
import { ApiError, api } from '@/lib/api'
import { useUser, type UserEnvelope } from '@/lib/user'

export default function Questionnaire() {
  const navigate = useNavigate()
  const { setUser } = useUser()
  const [index, setIndex] = useState(0)
  const [answers, setAnswers] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const question = questionnaire.questions[index]
  const answer = answers[question.id] ?? ''
  const last = index === questionnaire.questions.length - 1

  async function submit() {
    setBusy(true)
    setError(null)
    try {
      const reply = await api<UserEnvelope>('/api/questionnaire', {
        method: 'POST',
        body: JSON.stringify({
          version: questionnaire.version,
          answers: questionnaire.questions.map((q) => ({
            question: q.prompt,
            answer: (answers[q.id] ?? '').trim(),
          })),
        }),
      })
      setUser(reply.user)
      navigate('/home', { replace: true })
    } catch (err) {
      // Session expired mid-flow: drop the user to login instead of stranding them.
      if (err instanceof ApiError && err.status === 401) {
        setUser(null)
        return
      }
      setError(err instanceof Error ? err.message : 'Could not save your answers.')
      setBusy(false)
    }
  }

  return (
    <main className="grid min-h-dvh place-items-center px-4 py-8">
      <Card className="w-full max-w-md gap-5 bg-card/90 backdrop-blur">
        <CardHeader className="gap-2">
          <p className="text-xs uppercase tracking-[0.16em] text-primary">
            Question {index + 1} of {questionnaire.questions.length}
          </p>
          <CardTitle className="text-xl tracking-tight">{questionnaire.title}</CardTitle>
          <CardDescription>{questionnaire.intro}</CardDescription>
        </CardHeader>

        <CardContent>
          <form
            className="flex flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (!answer.trim()) return
              if (last) {
                void submit()
              } else {
                setIndex(index + 1)
              }
            }}
          >
            <div className="flex flex-col gap-1.5">
              <Label htmlFor={question.id}>{question.prompt}</Label>
              <Input
                id={question.id}
                data-testid="questionnaire-answer"
                value={answer}
                onChange={(e) => setAnswers({ ...answers, [question.id]: e.target.value })}
                placeholder={question.placeholder}
                autoFocus
                required
              />
            </div>

            <div className="flex items-center justify-between gap-3">
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={index === 0 || busy}
                onClick={() => setIndex(index - 1)}
              >
                Back
              </Button>
              <Button
                type="submit"
                data-testid={last ? 'questionnaire-submit' : 'questionnaire-next'}
                disabled={busy || !answer.trim()}
              >
                {busy ? 'Saving…' : last ? 'Finish setup' : 'Next'}
              </Button>
            </div>
            {error && (
              <Alert variant="destructive">
                <AlertDescription className="text-destructive">{error}</AlertDescription>
              </Alert>
            )}
          </form>
        </CardContent>
      </Card>
    </main>
  )
}
