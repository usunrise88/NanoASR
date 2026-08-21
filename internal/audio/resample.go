package audio

// Resample converts between sample rates on the native decode path.
//
// It must be a band-limited (windowed-sinc / polyphase) resampler, not linear
// interpolation: the primary workload is 8 kHz telephony upsampled to 16 kHz,
// and linear interpolation there adds aliasing that measurably costs accuracy.
//
// The ffmpeg path does not use this — it asks soxr for the same thing. The
// sherpa-onnx recogniser also resamples internally inside AcceptWaveform, but
// VAD and diarization take 16 kHz only, so the pipeline normalises up front.
//
// TODO(M5): implement Kaiser-windowed sinc with a precomputed polyphase filter
// bank; test against a sine sweep for THD and against ffmpeg for parity.
func Resample(in []float32, from, to int) []float32 {
	if from == to || len(in) == 0 {
		return in
	}
	panic("audio.Resample: not implemented (M5)")
}
