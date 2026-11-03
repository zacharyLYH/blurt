import { useState } from 'react'
import type { FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Questionnaire,
  QuestionnaireActions,
  QuestionnaireChoice,
  QuestionnaireChoices,
  QuestionnaireDescription,
  QuestionnaireInput,
  QuestionnaireItem,
  QuestionnaireNext,
  QuestionnairePrevious,
  QuestionnaireProgress,
  QuestionnaireSkip,
  QuestionnaireSubmit,
  QuestionnaireTitle,
} from '@/components/ui/questionnaire'
import questionnaireData from '@/data/questionnaire/v1.json'
import { ApiError, api } from '@/lib/api'
import { useUser, type UserEnvelope } from '@/lib/user'

type Choice = { value: string; label: string }

type Item = {
  id: string
  kind: 'text' | 'timeRange' | 'choice'
  prompt: string
  description?: string
  placeholder?: string
  required?: boolean
  default?: string
  defaults?: string[]
  labels?: string[]
  choices?: Choice[]
}

const questionnaire = questionnaireData as {
  version: number
  title: string
  intro: string
  items: Item[]
}

const definitions = questionnaire.items.map((item) => ({
  name: item.id,
  required: item.required !== false,
  choices: item.choices?.map((choice) => ({ value: choice.value })),
}))

// Time inputs speak HH:MM. The answer sent to Memory is the sentence a reader
// (and the planner) can use, not the raw control value.
function rangeAnswer(item: Item, values: string[]): string {
  const [start = '', end = ''] = values
  if (!start || !end) return ''
  if (item.id === 'waking') return `Wake ${start}, sleep ${end}`
  if (item.id === 'quiet') return `Quiet from ${start} until ${end}`
  return `${start}–${end}`
}

function choiceAnswer(item: Item, value: string): string {
  return item.choices?.find((choice) => choice.value === value)?.label ?? value
}

function readAnswers(form: FormData): { question: string; answer: string }[] {
  return questionnaire.items.flatMap((item) => {
    const answer =
      item.kind === 'timeRange'
        ? rangeAnswer(item, form.getAll(item.id).map(String))
        : item.kind === 'choice'
          ? choiceAnswer(item, String(form.get(item.id) ?? ''))
          : String(form.get(item.id) ?? '').trim()
    return answer ? [{ question: item.prompt, answer }] : []
  })
}

export default function QuestionnairePage() {
  const navigate = useNavigate()
  const { setUser } = useUser()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const reply = await api<UserEnvelope>('/api/questionnaire', {
        method: 'POST',
        body: JSON.stringify({
          version: questionnaire.version,
          answers: readAnswers(new FormData(event.currentTarget)),
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
      <Card className="w-full max-w-lg gap-5 bg-card/90 backdrop-blur sm:p-8">
        <CardHeader className="gap-1.5">
          <CardTitle className="text-xl tracking-tight">{questionnaire.title}</CardTitle>
          <CardDescription>{questionnaire.intro}</CardDescription>
        </CardHeader>

        <CardContent>
          <Questionnaire
            defaultItem={questionnaire.items[0]?.id}
            items={definitions}
            shortcuts="numbers"
            onSubmit={submit}
            className="gap-5"
          >
            <QuestionnaireProgress data-testid="questionnaire-progress" />

            {questionnaire.items.map((item) => {
              const required = item.required !== false
              return (
                <QuestionnaireItem
                  key={item.id}
                  name={item.id}
                  required={required}
                  multiple={item.kind === 'timeRange'}
                  className="flex flex-col gap-3"
                >
                  <QuestionnaireTitle>{item.prompt}</QuestionnaireTitle>
                  {item.description && (
                    <QuestionnaireDescription>{item.description}</QuestionnaireDescription>
                  )}

                  {item.kind === 'timeRange' && (
                    <div className="grid grid-cols-2 gap-3">
                      {(item.defaults ?? ['', '']).map((value, index) => (
                        <label key={item.labels?.[index] ?? index} className="flex flex-col gap-1.5">
                          <span className="text-xs font-medium text-muted-foreground">
                            {item.labels?.[index] ?? (index === 0 ? 'From' : 'To')}
                          </span>
                          <QuestionnaireInput
                            type="time"
                            defaultValue={value}
                            data-testid={`questionnaire-${item.id}-${index}`}
                          />
                        </label>
                      ))}
                    </div>
                  )}

                  {item.kind === 'choice' && (
                    <QuestionnaireChoices>
                      {item.choices?.map((choice) => (
                        <QuestionnaireChoice
                          key={choice.value}
                          value={choice.value}
                          defaultChecked={choice.value === item.default}
                          data-testid={`questionnaire-${item.id}-${choice.value}`}
                        >
                          {choice.label}
                        </QuestionnaireChoice>
                      ))}
                    </QuestionnaireChoices>
                  )}

                  {item.kind === 'text' && (
                    <QuestionnaireInput
                      type="text"
                      placeholder={item.placeholder}
                      data-testid={`questionnaire-${item.id}`}
                    />
                  )}
                </QuestionnaireItem>
              )
            })}

            {error && (
              <Alert variant="destructive">
                <AlertDescription className="text-destructive">{error}</AlertDescription>
              </Alert>
            )}

            <QuestionnaireActions>
              <QuestionnairePrevious data-testid="questionnaire-back" />
              <QuestionnaireSkip data-testid="questionnaire-skip" />
              <QuestionnaireNext data-testid="questionnaire-next" disabled={busy} />
              <QuestionnaireSubmit data-testid="questionnaire-submit" disabled={busy}>
                {busy ? 'Saving…' : 'Finish setup'}
              </QuestionnaireSubmit>
            </QuestionnaireActions>
          </Questionnaire>
        </CardContent>
      </Card>
    </main>
  )
}
