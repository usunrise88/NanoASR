/**
 * The waveform envelope: RMS over fixed windows, one value per drawn column.
 *
 * RMS rather than peak amplitude because a single clipped sample makes a peak
 * plot look like solid speech; RMS follows loudness, which is what a person
 * reading a waveform is actually looking for.
 */
export function rmsEnvelope(samples: Float32Array, buckets: number): Float32Array {
  const out = new Float32Array(buckets)
  if (buckets <= 0 || samples.length === 0) return out

  const width = samples.length / buckets
  for (let i = 0; i < buckets; i++) {
    const start = Math.floor(i * width)
    const end = Math.min(samples.length, Math.max(start + 1, Math.floor((i + 1) * width)))

    let sum = 0
    for (let j = start; j < end; j++) {
      const v = samples[j] ?? 0
      sum += v * v
    }
    out[i] = Math.sqrt(sum / (end - start))
  }
  return out
}

/**
 * Scales an envelope so the loudest column reaches 1.
 *
 * Telephony audio is often recorded quiet, and an un-normalised waveform of it
 * is a flat line with the speech invisible inside it. The peak is found rather
 than assumed, so a clip that is already loud is left alone.
 */
export function normalise(envelope: Float32Array): Float32Array {
  let peak = 0
  for (const v of envelope) if (v > peak) peak = v
  if (peak <= 0) return envelope

  const out = new Float32Array(envelope.length)
  for (let i = 0; i < envelope.length; i++) out[i] = (envelope[i] ?? 0) / peak
  return out
}

/** Averages channels into one. The waveform shows the file, not one side of it. */
export function downmix(channels: Float32Array[]): Float32Array {
  const first = channels[0]
  if (!first) return new Float32Array(0)
  if (channels.length === 1) return first

  const out = new Float32Array(first.length)
  for (let i = 0; i < out.length; i++) {
    let sum = 0
    for (const channel of channels) sum += channel[i] ?? 0
    out[i] = sum / channels.length
  }
  return out
}
