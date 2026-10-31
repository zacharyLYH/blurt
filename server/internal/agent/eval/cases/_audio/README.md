# L3 audio fixtures

One short clip per file, in the shape the browser produces: mono 16 kHz PCM
WAV with the canonical 44-byte header (`capture.SniffAudio` accepts it).

Regenerate on macOS, then strip `say`'s `JUNK` chunk so the header lands at the
offsets the server expects:

```sh
say -o /tmp/glue.wav --data-format=LEI16@16000 "Remind me to buy glue on the way home."
python3 - <<'PY'
import struct
raw = open('/tmp/glue.wav','rb').read()
pos, chunks = 12, {}
while pos + 8 <= len(raw):
    cid = raw[pos:pos+4]; size = struct.unpack('<I', raw[pos+4:pos+8])[0]
    chunks[cid] = raw[pos+8:pos+8+size]
    pos += 8 + size + (size & 1)
fmt, data = chunks[b'fmt '], chunks[b'data']
open('glue.wav','wb').write(b'RIFF' + struct.pack('<I', 36 + len(data)) + b'WAVE' +
    b'fmt ' + struct.pack('<I', len(fmt)) + fmt + b'data' + struct.pack('<I', len(data)) + data)
PY
```

The clip is only as good as its transcript: check it against the real gateway
before committing a new one (`skills/voice-capture` has the `/v1/infer` body).
A case pins the words it needs with `transcript_has`, never the whole sentence
phonon returns.

`silence.wav` is the exception: 400 ms of zeros, written by hand because `say`
cannot record nothing. It pins the blank-transcript path — the Job closes with
the fallback sentence and writes no rows — so it asserts `kind: failed`, not words.
