package audio

import "encoding/binary"

// Decoding a raw stream, as opposed to a file.
//
// The realtime API is handed bare samples with no container around them: the
// client said what the format is when it opened the session, and every frame
// after that is more of it. There is no header to sniff and no length to
// check, so these are deliberately the smallest functions that do the
// conversion — the same arithmetic the WAV decoder does per sample, over a
// whole buffer.

// DecodePCM16LE converts signed 16-bit little-endian samples.
//
// It returns the trailing byte when the buffer holds an odd number of them,
// for the caller to prepend to the next one. A websocket frame boundary has
// nothing to do with a sample boundary, and dropping that byte would shift
// every later sample by eight bits — which is not silence, it is noise.
func DecodePCM16LE(b []byte) (samples []float32, rest []byte) {
	n := len(b) / 2
	samples = make([]float32, n)
	for i := range n {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768
	}
	if len(b)%2 == 1 {
		return samples, b[len(b)-1:]
	}
	return samples, nil
}

// DecodeULaw converts G.711 mu-law, the telephony encoding of North America
// and Japan. One byte per sample, so there is never a remainder.
func DecodeULaw(b []byte) []float32 { return companded(b, &ulawTable) }

// DecodeALaw converts G.711 A-law, the telephony encoding of most of the rest
// of the world.
func DecodeALaw(b []byte) []float32 { return companded(b, &alawTable) }

func companded(b []byte, table *[256]int16) []float32 {
	out := make([]float32, len(b))
	for i, v := range b {
		out[i] = float32(table[v]) / 32768
	}
	return out
}
