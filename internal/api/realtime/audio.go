package realtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// inputFormat is what the client said its bytes are.
//
// There is no header to sniff here. The realtime protocol has the client
// declare the format once and send bare samples afterwards, so a wrong
// declaration does not fail — it transcribes noise. That is why the accepted
// set is small and the rates are bounded.
type inputFormat struct {
	// name is the spelling echoed back in the session object.
	name string
	rate int
	// decode converts one frame and returns the bytes that did not form a
	// whole sample, for the next frame to start with.
	decode func([]byte) ([]float32, []byte)
}

const (
	// OpenAI's realtime API is 24 kHz PCM16 mono by default, and a client that
	// does not say otherwise means that.
	defaultPCMRate = 24000
	minRate        = 8000
	maxRate        = 48000
)

func pcm16(rate int) inputFormat {
	return inputFormat{name: "pcm16", rate: rate, decode: audio.DecodePCM16LE}
}

func ulaw() inputFormat {
	return inputFormat{
		name: "g711_ulaw", rate: 8000,
		decode: func(b []byte) ([]float32, []byte) { return audio.DecodeULaw(b), nil },
	}
}

func alaw() inputFormat {
	return inputFormat{
		name: "g711_alaw", rate: 8000,
		decode: func(b []byte) ([]float32, []byte) { return audio.DecodeALaw(b), nil },
	}
}

func defaultFormat() inputFormat { return pcm16(defaultPCMRate) }

// formatObject is the newer shape of input_audio_format, which carries the
// rate instead of implying it.
type formatObject struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

// parseFormat accepts both spellings: the string enum of the beta protocol and
// the object of the GA one.
//
// The object form matters more here than it does to OpenAI. Their models are
// 24 kHz and a client resamples to meet them; the models here are 8 and
// 16 kHz, telephony audio arrives at 8 kHz already, and making every caller
// resample to 24 kHz only to have sherpa-onnx resample it back would cost
// quality for nothing.
func parseFormat(raw json.RawMessage) (inputFormat, error) {
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return formatByName(name, 0)
	}

	var obj formatObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return inputFormat{}, core.Errorf(core.CodeInvalidRequest,
			"input_audio_format must be a string or an object with type and rate").
			WithParam("input_audio_format")
	}
	return formatByName(obj.Type, obj.Rate)
}

func formatByName(name string, rate int) (inputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "pcm16", "audio/pcm", "pcm", "pcm_s16le":
		if rate == 0 {
			rate = defaultPCMRate
		}
		if rate < minRate || rate > maxRate {
			return inputFormat{}, core.Errorf(core.CodeInvalidRequest,
				"input_audio_format rate must be between %d and %d Hz, got %d",
				minRate, maxRate, rate).WithParam("input_audio_format")
		}
		return pcm16(rate), nil
	case "g711_ulaw", "audio/pcmu", "pcmu", "mulaw":
		return ulaw(), nil
	case "g711_alaw", "audio/pcma", "pcma", "alaw":
		return alaw(), nil
	default:
		return inputFormat{}, core.Errorf(core.CodeInvalidRequest,
			"input_audio_format %q is not supported: use pcm16, g711_ulaw or g711_alaw",
			name).WithParam("input_audio_format")
	}
}

// String is what the session object reports, including the rate when it is not
// the one the format name implies.
func (f inputFormat) String() string {
	if f.name == "pcm16" && f.rate != defaultPCMRate {
		return fmt.Sprintf("pcm16;rate=%d", f.rate)
	}
	return f.name
}
