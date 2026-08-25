/// <reference lib="webworker" />

import { downmix, normalise, rmsEnvelope } from './peaks'

/**
 * Reduces decoded samples to the waveform envelope, off the main thread.
 *
 * Decoding happens in the caller, not here: OfflineAudioContext is a window
 * interface and does not exist in a worker, so constructing one inside a worker
 * is a ReferenceError. What is left is the part that genuinely benefits from
 * being off the main thread — one pass over several million samples — and the
 * samples arrive transferred rather than copied.
 */
export interface PeaksRequest {
  channels: Float32Array[]
  /** How many columns the waveform will draw. */
  buckets: number
  duration: number
}

export interface PeaksResponse {
  ok: true
  peaks: Float32Array
  duration: number
}

export interface PeaksFailure {
  ok: false
  error: string
}

self.onmessage = (event: MessageEvent<PeaksRequest>) => {
  const { channels, buckets, duration } = event.data
  try {
    const peaks = normalise(rmsEnvelope(downmix(channels), buckets))
    const response: PeaksResponse = { ok: true, peaks, duration }
    self.postMessage(response, [peaks.buffer])
  } catch (err) {
    const response: PeaksFailure = { ok: false, error: String(err) }
    self.postMessage(response)
  }
}
