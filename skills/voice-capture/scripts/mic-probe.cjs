// Records 800 ms through Blurt's capture graph and prints the encoded shape.
// Run from web/ after `npm ci` and `npx playwright install chromium`:
//
//   node ../skills/voice-capture/scripts/mic-probe.cjs
const http = require('node:http')
const { chromium } = require('playwright')

const server = http.createServer((_req, res) => {
  res.setHeader('content-type', 'text/html')
  res.end('<html><body>probe</body></html>')
})

server.listen(18090, '127.0.0.1', async () => {
  try {
    const browser = await chromium.launch({
      args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
    })
    const page = await browser.newPage()
    await page.goto('http://localhost:18090')
    const result = await page.evaluate(async () => {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      const ctx = new AudioContext({ sampleRate: 16000 })
      const destination = ctx.createMediaStreamDestination()
      destination.channelCount = 1
      destination.channelCountMode = 'explicit'
      ctx.createMediaStreamSource(stream).connect(destination)

      const recorder = new MediaRecorder(destination.stream, {
        mimeType: 'audio/webm;codecs=opus',
        audioBitsPerSecond: 24000,
      })
      const chunks = []
      recorder.ondataavailable = (event) => chunks.push(event.data)
      const stopped = new Promise((resolve) => {
        recorder.onstop = resolve
      })
      recorder.start()
      await new Promise((resolve) => setTimeout(resolve, 800))
      recorder.stop()
      await stopped

      const blob = new Blob(chunks, { type: recorder.mimeType })
      const bytes = new Uint8Array(await blob.arrayBuffer())
      return {
        contextRate: ctx.sampleRate,
        trackRate: destination.stream.getAudioTracks()[0].getSettings().sampleRate,
        type: blob.type,
        size: blob.size,
        hex: [...bytes.slice(0, 600)].map((b) => b.toString(16).padStart(2, '0')).join(''),
      }
    })
    console.log(JSON.stringify(result, null, 2))
    await browser.close()
    server.close()
  } catch (err) {
    console.error(err)
    server.close()
    process.exit(1)
  }
})
