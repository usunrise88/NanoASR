package audio

import (
	"encoding/binary"
	"io"
	"math"
)

// WAVHeaderSize is the length of the header WAVHeader writes.
const WAVHeaderSize = 44

// WAVHeader is the canonical 44-byte header of a mono 16-bit PCM file holding
// dataBytes of samples. The isolated voice is served this way: a header in
// front of PCM already on disk, so a long recording streams from the file
// rather than being encoded in memory first.
func WAVHeader(sampleRate int, dataBytes int64) []byte {
	const channels, bits = 1, 16
	h := make([]byte, WAVHeaderSize)
	le := binary.LittleEndian
	copy(h[0:], "RIFF")
	le.PutUint32(h[4:], uint32(min(dataBytes+WAVHeaderSize-8, math.MaxUint32)))
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1) // PCM
	le.PutUint16(h[22:], channels)
	le.PutUint32(h[24:], uint32(sampleRate))
	le.PutUint32(h[28:], uint32(sampleRate*channels*bits/8))
	le.PutUint16(h[32:], channels*bits/8)
	le.PutUint16(h[34:], bits)
	copy(h[36:], "data")
	le.PutUint32(h[40:], uint32(min(dataBytes, math.MaxUint32)))
	return h
}

// EncodePCM16 converts samples to little-endian 16-bit PCM, clipping what
// does not fit rather than wrapping it.
func EncodePCM16(samples []float32) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		v := math.Round(float64(s) * 32767)
		v = max(-32768, min(32767, v))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(v)))
	}
	return out
}

// WriteWAV writes samples as a whole mono 16-bit WAV file.
func WriteWAV(w io.Writer, sampleRate int, samples []float32) error {
	data := EncodePCM16(samples)
	if _, err := w.Write(WAVHeader(sampleRate, int64(len(data)))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
