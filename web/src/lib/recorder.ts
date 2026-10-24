// Browser capture. The gateway takes exactly one shape — mono 16 kHz PCM WAV —
// so this module opens the mic once, forces a 16 kHz mono graph, and rewrites
// whatever the browser encoded into that WAV before uploading.

export type MicState = 'ready' | 'denied' | 'unavailable' | 'unsupported'

export interface Recording {
  blob: Blob
  durationMs: number
}

export interface MicSession {
  start(): void
  stop(): Promise<Recording>
  // loudness is the current input level, 0..1, for the capture UI.
  loudness(): number
  close(): void
}

const MIME_TYPES = ['audio/webm;codecs=opus', 'audio/ogg;codecs=opus']
const SAMPLE_RATE = 16_000
const BITRATE = 24_000
const MIN_DURATION_MS = 300
// WAV_HEADER_BYTES is the canonical 44-byte RIFF header the server sniffs.
const WAV_HEADER_BYTES = 44

// MicError carries the state the UI should fall back to. Plain errors are
// recording/verification failures, where the mic itself is still fine.
export class MicError extends Error {
  readonly state: MicState

  constructor(message: string, state: MicState) {
    super(message)
    this.name = 'MicError'
    this.state = state
  }
}

export async function openMicrophone(): Promise<MicSession> {
  if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === 'undefined') {
    throw new MicError('Voice capture is not supported here. Type instead.', 'unsupported')
  }
  const mimeType = MIME_TYPES.find((type) => MediaRecorder.isTypeSupported(type))
  if (!mimeType) {
    throw new MicError('This browser cannot record Opus. Type instead.', 'unsupported')
  }

  let stream: MediaStream
  try {
    stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: { ideal: 1 },
        sampleRate: { ideal: SAMPLE_RATE },
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    })
  } catch (err) {
    throw micError(err)
  }

  // The recorder encodes the graph's output, so pinning the context to 16 kHz
  // is what makes the blob match the contract. A browser that refuses gets the
  // text fallback rather than an upload the server will reject.
  let context: AudioContext
  try {
    context = new AudioContext({ sampleRate: SAMPLE_RATE })
  } catch {
    stream.getTracks().forEach((track) => track.stop())
    throw new MicError('This browser cannot record Opus. Type instead.', 'unsupported')
  }
  if (context.sampleRate !== SAMPLE_RATE) {
    stream.getTracks().forEach((track) => track.stop())
    void context.close()
    throw new MicError(`This browser records at ${context.sampleRate} Hz. Type instead.`, 'unsupported')
  }
  // A context created before the page was interacted with starts suspended,
  // and a suspended context records silence rather than failing. Chrome lifts
  // this on the first interaction; asking here covers the remaining cases.
  if (context.state === 'suspended') {
    await context.resume().catch(() => undefined)
  }
  const destination = context.createMediaStreamDestination()
  // MediaStreamAudioDestination defaults to stereo. Force mono here so the
  // encoded Opus track matches the contract (Chrome otherwise writes 2ch).
  destination.channelCount = 1
  destination.channelCountMode = 'explicit'
  // The analyser taps the same path, so the capture UI can draw a level
  // without opening a second microphone stream.
  const analyser = context.createAnalyser()
  analyser.fftSize = 1024
  const source = context.createMediaStreamSource(stream)
  source.connect(analyser)
  analyser.connect(destination)
  return new OpusRecorder(stream, context, destination, mimeType, analyser)
}

class OpusRecorder implements MicSession {
  private readonly stream: MediaStream
  private readonly context: AudioContext
  private readonly destination: MediaStreamAudioDestinationNode
  private readonly mimeType: string
  private readonly analyser: AnalyserNode
  private readonly samples: Float32Array<ArrayBuffer>
  private recorder: MediaRecorder | null = null
  private stopping: Promise<Recording> | null = null
  private chunks: Blob[] = []
  private startedAt = 0

  constructor(
    stream: MediaStream,
    context: AudioContext,
    destination: MediaStreamAudioDestinationNode,
    mimeType: string,
    analyser: AnalyserNode,
  ) {
    this.stream = stream
    this.context = context
    this.destination = destination
    this.mimeType = mimeType
    this.analyser = analyser
    this.samples = new Float32Array(analyser.fftSize)
  }

  // loudness reads the time-domain window and reports its RMS, lifted well
  // above full scale: speech RMS is small, so a raw value would barely move.
  loudness(): number {
    this.analyser.getFloatTimeDomainData(this.samples)
    let sum = 0
    for (const sample of this.samples) sum += sample * sample
    return Math.min(1, Math.sqrt(sum / this.samples.length) * 6)
  }

  start() {
    if (this.recorder) throw new Error('Already recording.')
    // Ask again inside the tap: the context may have started suspended.
    void this.context.resume()
    this.chunks = []
    this.startedAt = performance.now()
    const recorder = new MediaRecorder(this.destination.stream, {
      mimeType: this.mimeType,
      audioBitsPerSecond: BITRATE,
    })
    recorder.ondataavailable = (event) => {
      if (event.data.size > 0) this.chunks.push(event.data)
    }
    this.recorder = recorder
    recorder.start()
  }

  stop(): Promise<Recording> {
    const recorder = this.recorder
    if (!recorder) {
      // A second stop while one is still finishing joins it rather than
      // rejecting, so the countdown cap and the Stop button cannot race.
      return this.stopping ?? Promise.reject(new Error('Not recording.'))
    }
    this.recorder = null

    this.stopping = new Promise<Recording>((resolve, reject) => {
      recorder.onerror = () => reject(new Error('The recording failed. Try again.'))
      recorder.onstop = () => {
        const durationMs = performance.now() - this.startedAt
        const blob = new Blob(this.chunks, { type: this.mimeType })
        this.chunks = []
        if (durationMs < MIN_DURATION_MS) {
          reject(new Error('That was too short. Hold the mic a little longer.'))
          return
        }
        toWav(this.context, blob).then((wav) => resolve({ blob: wav, durationMs }), reject)
      }
      recorder.stop()
    }).finally(() => {
      this.stopping = null
    })
    return this.stopping
  }

  close() {
    if (this.recorder) {
      try {
        this.recorder.stop()
      } catch {
        // Already stopped by a page transition; tracks below are what matter.
      }
      this.recorder = null
    }
    this.stream.getTracks().forEach((track) => track.stop())
    void this.context.close()
  }
}

// toWav rewrites whatever the browser encoded into the one shape the gateway
// accepts. The graph is pinned to 16 kHz mono, so decodeAudioData resamples to
// exactly that and nothing else reaches the wire. It also means the recorder's
// container choice stops mattering: only this function has to decode it.
async function toWav(context: AudioContext, blob: Blob): Promise<Blob> {
  const decoded = await context.decodeAudioData(await blob.arrayBuffer())
  if (decoded.numberOfChannels !== 1 || decoded.sampleRate !== SAMPLE_RATE) {
    throw new Error(
      `The recorder used ${decoded.numberOfChannels} channel(s) at ${decoded.sampleRate} Hz. Type instead.`,
    )
  }
  const samples = decoded.getChannelData(0)
  const buffer = new ArrayBuffer(WAV_HEADER_BYTES + samples.length * 2)
  const view = new DataView(buffer)
  writeAscii(view, 0, 'RIFF')
  view.setUint32(4, buffer.byteLength - 8, true)
  writeAscii(view, 8, 'WAVE')
  writeAscii(view, 12, 'fmt ')
  view.setUint32(16, 16, true) // fmt chunk size
  view.setUint16(20, 1, true) // PCM
  view.setUint16(22, 1, true) // mono
  view.setUint32(24, SAMPLE_RATE, true)
  view.setUint32(28, SAMPLE_RATE * 2, true) // byte rate: 16-bit mono
  view.setUint16(32, 2, true) // block align
  view.setUint16(34, 16, true) // bits per sample
  writeAscii(view, 36, 'data')
  view.setUint32(40, samples.length * 2, true)
  for (let i = 0; i < samples.length; i++) {
    const sample = Math.max(-1, Math.min(1, samples[i]))
    view.setInt16(WAV_HEADER_BYTES + i * 2, Math.round(sample * (sample < 0 ? 0x8000 : 0x7fff)), true)
  }
  return new Blob([buffer], { type: 'audio/wav' })
}

// writeAscii lays out a four-character chunk id.
function writeAscii(view: DataView, offset: number, text: string) {
  for (let i = 0; i < text.length; i++) view.setUint8(offset + i, text.charCodeAt(i))
}

function micError(err: unknown): MicError {
  if (err instanceof DOMException) {
    if (err.name === 'NotAllowedError' || err.name === 'SecurityError') {
      return new MicError('Microphone permission is off. Type instead.', 'denied')
    }
    if (err.name === 'NotFoundError' || err.name === 'OverconstrainedError') {
      return new MicError('No microphone found. Type instead.', 'unavailable')
    }
  }
  return new MicError('Microphone unavailable. Type instead.', 'unavailable')
}
