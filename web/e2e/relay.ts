// A fake push service for the specs that exercise the real sender. The Go container POSTs
// to a server on the host through host.docker.internal, exactly as it would to FCM or
// Apple, so the encrypted body, the VAPID signature, and the pruning behaviour stay
// observable.
//
// The endpoint must be plain http for a host relay, which the subscription guard allows for
// localhost and host.docker.internal only.
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'

export interface Captured {
  headers: IncomingMessage['headers']
  body: Buffer
}

// startRelay answers every push with one status and records what arrived.
export async function startRelay(status: number) {
  const captured: Captured[] = []
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = []
    req.on('data', (chunk: Buffer) => chunks.push(chunk))
    req.on('end', () => {
      captured.push({ headers: req.headers, body: Buffer.concat(chunks) })
      res.writeHead(status).end()
    })
  })
  await new Promise<void>((resolve) => server.listen(0, '0.0.0.0', resolve))
  const { port } = server.address() as { port: number }
  return {
    endpoint: `http://host.docker.internal:${port}/push`,
    captured,
    close: () =>
      new Promise<void>((resolve) => {
        // The sender keeps connections alive, so close() alone would wait.
        server.closeAllConnections()
        server.close(() => resolve())
      }),
  }
}

// withRelay runs a test body against a relay that answers every push with one status, and
// always shuts it down: a body cannot forget the teardown.
export async function withRelay(status: number, run: (endpoint: string, captured: Captured[]) => Promise<void>) {
  const relay = await startRelay(status)
  try {
    await run(relay.endpoint, relay.captured)
  } finally {
    await relay.close()
  }
}