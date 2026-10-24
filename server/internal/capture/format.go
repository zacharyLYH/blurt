// Package capture owns the audio ingress: checking the blob is the mono 16 kHz
// WAV shape Blurt promises, keeping it on disk only while a Job needs it, and
// running the transcription pass with retries and a user-facing fallback.
package capture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	// HeaderBytes is the canonical RIFF header. Only the header is held in
	// memory; the rest of the blob streams to disk.
	HeaderBytes = 44
	// MaxUploadBytes bounds one blob. A minute of the WAV capture Blurt sends is
	// ~1.9 MB, so this clears it with room to spare without giving STT an
	// unbounded input.
	MaxUploadBytes = 2 << 20
	// MinDuration rejects a tap that produced no speech. MaxDuration is the
	// spend guard the PRD asks for, and matches the client's own countdown.
	MinDuration = 300 * time.Millisecond
	MaxDuration = time.Minute
)

// ErrFormat means the blob is not the mono 16 kHz WAV Blurt promises.
var ErrFormat = errors.New("audio must be mono 16 kHz WAV")

// Format is the decoded shape of the blob. WAV declares its real capture rate,
// so both fields are read from the header rather than inferred.
type Format struct {
	SampleRate int
	Channels   int
}

// SniffAudio reads only the header. The recorder writes a canonical RIFF layout
// at fixed offsets — fmt at 12, data at 36 — so a WAV from any other writer
// (ffmpeg's default, with a LIST chunk between them, is a real example) fails
// the offsets rather than being parsed loosely. The recorder is the only writer.
func SniffAudio(header []byte) (Format, error) {
	const (
		fmtAt  = 12
		dataAt = 36
		need   = dataAt + 8
	)
	if len(header) < need || !bytes.HasPrefix(header, []byte("RIFF")) ||
		string(header[8:12]) != "WAVE" ||
		string(header[fmtAt:fmtAt+4]) != "fmt " || string(header[dataAt:dataAt+4]) != "data" {
		return Format{}, ErrFormat
	}
	// PCM (format tag 1) at 16 bits is what the recorder encodes; anything else
	// would change the byte rate the upload cap is sized against.
	if binary.LittleEndian.Uint16(header[20:22]) != 1 || binary.LittleEndian.Uint16(header[34:36]) != 16 {
		return Format{}, ErrFormat
	}
	format := Format{
		Channels:   int(binary.LittleEndian.Uint16(header[22:24])),
		SampleRate: int(binary.LittleEndian.Uint32(header[24:28])),
	}
	if format.Channels != 1 || format.SampleRate != 16000 {
		return Format{}, fmt.Errorf("%w: got %d Hz, %d channel(s)", ErrFormat,
			format.SampleRate, format.Channels)
	}
	return format, nil
}
