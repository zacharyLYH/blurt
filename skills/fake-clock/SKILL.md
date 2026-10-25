---
name: fake-clock
description: Make time-dependent backend behaviour testable end to end when the language reads the clock itself. Use when a test needs a scheduled instant to arrive, a periodic worker to run, or a session to expire, and waiting for the real clock is too slow.
---

# Crossing a long horizon in a test

Blurt's emitter fires a Reminder when its instant arrives. A reminder a day out cannot be
exercised by waiting, so the clock has to move. Two of the obvious moves do not work.

## What does not work (checked, not assumed)

- **`LD_PRELOAD` + libfaketime.** Go reads the clock through the vDSO, so `time.Now` never
  reaches libc; the preload is silently ignored.
- **`date -s` inside the container.** It needs `CAP_SYS_TIME`, which compose does not grant.

## What works: three levers, in increasing cost

1. **Take the clock as a field.** `agent.AgentLoop.Now` and `emitter.Emitter.Now` are
   `func() time.Time` fields; nil means `time.Now`. Go tests set them and drive one step
   (`Emitter.sweep`) at a time, which is where the exhaustive cases belong: horizons,
   downtime catch-up, restart, spacing, bad rows.
2. **A dev-only offset from the environment**, applied where the clock is read, mirroring the
   other escape hatches (`BLURT_FAKE_PIN`, `BLURT_FAKE_STT`, `BLURT_FAKE_AGENT`):
   `config.FakeClock` → `main.go` wraps `time.Now` for the emitter only, and logs a warning
   because it is fatal to leave on in production. Interpolate it in the compose file
   (`BLURT_FAKE_CLOCK: "${BLURT_FAKE_CLOCK:-90s}"`) so one spec can override it for its own run
   while every other run keeps the default.
3. **Restart the container with the offset raised.** This is also the only way to test the
   wiring rather than the logic: a fresh process must pick up what the last one left due, and
   must not re-fire what it already fired. Two traps:
   - Recreate the proxy with the server (`up -d --force-recreate server web`). nginx resolves
     the server's address when it loads its config, so a server-only recreate leaves 502s.
   - Use async `execFile` from the spec, never `execFileSync`: the fake relay runs in the test
     process, and a blocking child stops it answering the very push the restart causes.
   Restore the default in a `finally`, so a failure cannot leave the stack shifted for the
   specs that follow, and the run's file order stops mattering.

## Checking a payload you cannot read

A push relay only ever sees ciphertext, so "bytes arrived" is a weak assertion. Mint the device
keys in the test, hand the public half to the backend through the subscription, and decrypt the
payload with the private half: `web/e2e/push-crypto.ts` is the receiver side of RFC 8291 over
RFC 8188.

Validate any change to such a helper against the RFC's own vector before trusting it — RFC 8291
Appendix A lists every intermediate value (ECDH secret, IKM, CEK, nonce) and the plaintext, which
turns "it fails to decrypt" into a one-run bisect. The usual mistake is the info strings: the CEK
and nonce info is the label plus a `0x00` terminator, with **no** counter octet — HKDF's expander
supplies its own counter internally, and adding one yields a plausible-looking key that never
authenticates.