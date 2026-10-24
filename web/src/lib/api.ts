// Thin fetch wrapper: same-origin, cookie credentials, JSON in and out.
// Errors keep the server's error code so callers can branch on stubs.

export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(message: string, status: number, code: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  // JSON helpers pass a string; a Blob keeps the caller's Content-Type (the
  // audio blob's own type), which is part of the capture contract.
  if (typeof init.body === 'string' && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const res = await fetch(path, { ...init, headers, credentials: 'include' })
  const text = await res.text()

  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = null
    }
  }

  if (!res.ok) {
    const failure = body as { error?: string; code?: string } | null
    throw new ApiError(
      failure?.error ?? `Request failed (${res.status})`,
      res.status,
      failure?.code ?? `http_${res.status}`,
    )
  }
  return body as T
}
