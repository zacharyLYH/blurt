package capture

import (
	"encoding/binary"
	"errors"
	"testing"
)

// wav writes the canonical 44-byte RIFF header the recorder emits. bits and tag
// are parameters so the rejection cases can vary them.
func wav(rate uint32, channels, bits, tag uint16) []byte {
	header := make([]byte, HeaderBytes)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], 36)
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], tag)
	binary.LittleEndian.PutUint16(header[22:], channels)
	binary.LittleEndian.PutUint32(header[24:], rate)
	binary.LittleEndian.PutUint16(header[34:], bits)
	copy(header[36:], "data")
	return header
}

// wavWithListChunk is what ffmpeg writes by default: a LIST chunk between fmt
// and data, so `data` is no longer at offset 36.
func wavWithListChunk() []byte {
	header := append([]byte{}, wav(16000, 1, 16, 1)[:36]...)
	return append(header, append([]byte("LIST"), make([]byte, 8)...)...)
}

func TestSniffAudioAcceptsRecorderOutput(t *testing.T) {
	format, err := SniffAudio(wav(16000, 1, 16, 1))
	if err != nil {
		t.Fatalf("SniffAudio: %v", err)
	}
	want := Format{SampleRate: 16000, Channels: 1}
	if format != want {
		t.Fatalf("format = %+v, want %+v", format, want)
	}
}

func TestSniffAudioRejectsAnythingElse(t *testing.T) {
	for name, blob := range map[string][]byte{
		// The graph is pinned to 16 kHz mono, so anything else is not a Blurt
		// capture, however valid a WAV it may be.
		"stereo":     wav(16000, 2, 16, 1),
		"48k":        wav(48000, 1, 16, 1),
		"float pcm":  wav(16000, 1, 32, 3),
		"8 bit pcm":  wav(16000, 1, 8, 1),
		"truncated":  wav(16000, 1, 16, 1)[:20],
		"empty":      {},
		"not a wave": append([]byte("RIFF1234NOPE"), make([]byte, 40)...),
		"odd layout": wavWithListChunk(),
		"ogg opus":   append([]byte("OggS"), make([]byte, 40)...),
		"webm opus":  append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 40)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SniffAudio(blob); !errors.Is(err, ErrFormat) {
				t.Fatalf("err = %v, want ErrFormat", err)
			}
		})
	}
}
