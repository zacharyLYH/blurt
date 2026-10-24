import { execFileSync } from 'node:child_process'
import { COMPOSE_PROJECT } from './env'

// Wipe the e2e stack and its throwaway volume so every run starts with an
// empty database. Best effort: a failed run must not leave containers behind.
export default function teardown() {
  try {
    execFileSync(
      'docker',
      [
        'compose',
        '-f', '../docker-compose.yml',
        '-f', '../docker-compose.e2e.yml',
        '-p', COMPOSE_PROJECT,
        'down', '-v', '--remove-orphans',
      ],
      { stdio: 'ignore' },
    )
  } catch {
    // Nothing to clean up, or docker went away mid-run.
  }
}
