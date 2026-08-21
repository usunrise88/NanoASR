package audio

import "math"

// G.711 companding tables.
//
// Telephony audio is A-law in most of the world and mu-law in North America and
// Japan, and both are per-sample transforms. A 256-entry table turns the
// decode into one array lookup instead of branching arithmetic on every sample
// of every minute of every call.
var (
	alawTable [256]int16
	ulawTable [256]int16
)

func init() {
	for i := 0; i < 256; i++ {
		alawTable[i] = alawToLinear(byte(i))
		ulawTable[i] = ulawToLinear(byte(i))
	}
}

func alawToLinear(a byte) int16 {
	a ^= 0x55 // A-law inverts alternate bits on the wire
	mantissa := int(a&0x0F) << 4
	exponent := (int(a) & 0x70) >> 4

	switch exponent {
	case 0:
		mantissa += 8
	case 1:
		mantissa += 0x108
	default:
		mantissa = (mantissa + 0x108) << (exponent - 1)
	}

	if a&0x80 != 0 {
		return int16(mantissa)
	}
	return int16(-mantissa)
}

func ulawToLinear(u byte) int16 {
	u = ^u
	mantissa := ((int(u&0x0F) << 3) + 0x84) << ((int(u) & 0x70) >> 4)
	mantissa -= 0x84

	if u&0x80 != 0 {
		return int16(-mantissa)
	}
	return int16(mantissa)
}

// float32frombits is math.Float32frombits, named locally so the sample
// conversion reads the same for every format.
func float32frombits(b uint32) float32 { return math.Float32frombits(b) }
