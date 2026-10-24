---
name: voice-capture
description: Debug browser capture and Playwright microphone tests for Blurt. Use when the blob is rejected as non-mono, the encoded rate looks wrong, a capture transcribes to nothing, or a headless mic test will not start.
---

# Voice capture

## The traps

1. **`MediaStreamAudioDestinationNode` is stereo by default.** Feeding it a mono mic still makes Chrome encode 2 channels, and `decodeAudioData` then hands back a 2-channel buffer. Force mono before connecting:
   ```ts
   destination.channelCount = 1
   destination.channelCountMode = 'explicit'
   ```
2. **The gateway refuses WebM, and Chrome cannot emit Ogg.** `MediaRecorder.isTypeSupported('audio/ogg;codecs=opus')` is **false** in Chromium (checked), so a Chrome capture is always WebM/Opus — which the transcription gateway answers with `400 invalid_audio`, listing wav/flac/ogg-opus/aiff/mp3 and telling you to convert first. That is why `stop()` decodes the blob and rewrites it as WAV. Do not "simplify" this by dropping the rewrite unless the gateway changes.
3. **The server accepts exactly one shape: mono 16 kHz PCM WAV.** `server/internal/capture/format.go` sniffs a canonical RIFF header at fixed offsets and nothing else; the Opus/WebM/Ogg parsers were deleted once the client stopped emitting them. `web/src/lib/recorder.ts` is the only place that should set the graph. Opus still codes at 48 kHz underneath, but nothing upstream cares any more: the rewrite normalises it away.

## The WAV rewrite

`toWav()` in `web/src/lib/recorder.ts` is the only place Opus is turned into PCM. Two facts make it short:

- `decodeAudioData` resamples to the **`AudioContext`'s** rate, so a 16 kHz context yields a 16 kHz `AudioBuffer` with no manual resampling.
- The server sniffs fixed RIFF offsets (fmt at 12, data at 36, tag=1, bits=16, channels at 22, rate at 24), so the header must be the canonical 44-byte layout. A `fact` chunk or any other ordering exists in valid WAVs and will be rejected.

A **suspended `AudioContext` records silence instead of failing**, so a capture that transcribes to nothing may be a context that never resumed. Chrome lifts suspension once the page has been interacted with; `openMicrophone` asks anyway.

## The gateway contract

Not OpenAI-compatible. `POST /v1/audio/transcriptions` answers `404 {"error":{"code":"not_found","message":"use POST /v1/infer"}}`.

```sh
curl -s https://<gateway>/v1/infer -H "Authorization: Bearer $KEY" \
  -H 'content-type: application/json' --data-binary @body.json
# {"model":"phonon","payload":{"audio":"data:audio/wav;base64,<...>"}}
# -> {"output": {"text": "..."}}
```

- `model` is validated: an unknown id is `404 unknown_model` with the `available` list. The audio model is `phonon`.
- Missing or wrong key is `401 unauthorized`, quickly. Blurt treats it as permanent, so a bad key never burns three retries.
- `timestamps: true` adds a `segments` list; Blurt omits it to keep the reply small.
- Latency varies a lot with clip length and host load: ~0.5 s for a few seconds, 4–30 s for a full minute. Blurt's client timeout is 120 s and the worker's attempt timeout 150 s.

## The permission gate eats your navigation

Blurt runs a permission gate on **every app entry** (App.tsx `Gated`), ahead of the questionnaire, because a browser never reports a revocation on its own. The "already checked" flag is module state, so it survives client-side routing but **not a full page load**.

That surprises tests: `page.goto(...)`, `page.reload()`, and even `signIn()` (whose first line is a `goto`) all start a new entry, so what used to land on `/home` or `/questionnaire` lands on `/permissions` instead. Symptom is an `expect(page).toHaveURL(/\/home$/)` timing out with `Received string: ".../permissions"`.

Use the helper, which is tolerant because the gate only shows while something still needs attention:

```ts
await passPermissions(page)   // waits briefly for Continue, clicks it if the gate is up
```

## The level tap

`loudness()` on the mic session returns RMS from an `AnalyserNode` inserted between the source and the destination, lifted ~6× (speech RMS is small). The `Boom` component samples it on `requestAnimationFrame` only while recording, rising fast and decaying at 0.88 per frame; that asymmetry is what reads as a breath instead of a flicker. No second microphone stream is opened for it.

## Probe without the app

From `web/` with Node 22+ and Playwright installed:

```sh
npm ci
npx playwright install chromium
npx playwright install-deps chromium   # Linux only; missing libglib is the usual failure
node ../skills/voice-capture/scripts/mic-probe.cjs
```

The probe opens Chromium with the fake mic, records 800 ms through the same 16 kHz mono graph, and prints `contextRate`, `trackRate`, `type`, `size`, and the first bytes. This is the **intermediate** encode, before the WAV rewrite — the server refuses it, so read it only to confirm the graph produced mono at the rate you asked for:

- WebM: `A_OPUS`, then `b5 84` (SamplingFrequency float) and `9f 81 01` (Channels = 1).
- Ogg: `OpusHead`, channel byte = 1, input rate 16000 or 48000.

What actually ships is the WAV: a 44-byte RIFF header with `fmt` at offset 12 and `data` at 36. To inspect a real one, read the blob the e2e stack left in its `blurt-data` volume — but the worker deletes it as soon as the Job is transcribed.

## Real browser e2e

In Playwright config, grant `permissions: ['microphone']` and launch with `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream`. To test the denial fallback, override `navigator.mediaDevices.getUserMedia` with an init script instead of changing permissions; `--use-fake-ui` auto-accepts otherwise.

An init script added before a `page.reload()` is the way to simulate a permission revoked between visits: `e2e/permissions.spec.ts` does that, and pins that the gate surfaces it without trapping the user.
