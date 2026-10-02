package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestDecodePCM16LEMatchesTheWAVReader(t *testing.T) {
	want := []int16{0, 1, -1, 32767, -32768, 1234}
	raw := make([]byte, 0, len(want)*2)
	for _, v := range want {
		raw = binary.LittleEndian.AppendUint16(raw, uint16(v))
	}

	got, rest := DecodePCM16LE(raw)
	if rest != nil {
		t.Errorf("an even-length buffer left %d bytes over", len(rest))
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d samples, want %d", len(got), len(want))
	}
	for i, v := range want {
		if expect := float32(v) / 32768; got[i] != expect {
			t.Errorf("sample %d is %v, want %v", i, got[i], expect)
		}
	}
}

// The case that matters for a stream: a frame that ends mid-sample. The odd
// byte has to come back so the next frame can start with it.
func TestDecodePCM16LECarriesAnOddByteOver(t *testing.T) {
	full := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}

	first, rest := DecodePCM16LE(full[:3])
	if len(first) != 1 {
		t.Fatalf("decoded %d samples from three bytes, want 1", len(first))
	}
	if len(rest) != 1 || rest[0] != 0x03 {
		t.Fatalf("leftover is %v, want [3]", rest)
	}

	second, rest := DecodePCM16LE(append(rest, full[3:]...))
	if len(second) != 2 || rest != nil {
		t.Fatalf("decoded %d samples with %d left over, want 2 and 0", len(second), len(rest))
	}

	// Decoding in two frames must give exactly what decoding the whole buffer
	// at once gives, which is the property a stream depends on.
	whole, _ := DecodePCM16LE(full)
	joined := append(first, second...)
	if len(joined) != len(whole) {
		t.Fatalf("split decode gave %d samples, whole gave %d", len(joined), len(whole))
	}
	for i := range whole {
		if joined[i] != whole[i] {
			t.Errorf("sample %d differs: split %v, whole %v", i, joined[i], whole[i])
		}
	}
}

func TestG711DecodesToTheSameValuesAsTheWAVPath(t *testing.T) {
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i)
	}

	for _, tc := range []struct {
		name  string
		got   []float32
		table *[256]int16
	}{
		{"ulaw", DecodeULaw(raw), &ulawTable},
		{"alaw", DecodeALaw(raw), &alawTable},
	} {
		if len(tc.got) != len(raw) {
			t.Fatalf("%s: decoded %d samples from %d bytes", tc.name, len(tc.got), len(raw))
		}
		for i := range raw {
			want := float32(tc.table[i]) / 32768
			if tc.got[i] != want {
				t.Errorf("%s: byte %d decoded to %v, want %v", tc.name, i, tc.got[i], want)
			}
			if math.Abs(float64(tc.got[i])) > 1 {
				t.Errorf("%s: byte %d decoded outside [-1,1]: %v", tc.name, i, tc.got[i])
			}
		}
	}
}
