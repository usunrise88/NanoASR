/**
 * Decodes a file into samples for the waveform.
 *
 * The rate is the whole trick: decodeAudioData resamples to the context's rate,
 * and ten minutes of 48 kHz stereo decoded at full rate is around 230 MB of
 * Float32 held in the tab. An RMS envelope of a couple of thousand columns
 * cannot tell the difference, and at 8 kHz the same file costs about 19 MB.
 *
 * This runs on the main thread because OfflineAudioContext is a window
 * interface — it does not exist in a worker. The decode itself is off-thread
 * inside the browser, so the cost here is the allocation, not a frozen page;
 * the numeric pass that follows is what goes to the worker.
 */
const RATE = 8000

export interface Decoded {
  channels: Float32Array[]
  duration: number
}

export async function decodeForWaveform(buffer: ArrayBuffer): Promise<Decoded> {
  // A one-frame context: decodeAudioData allocates its own buffer and only the
  // sample rate is being borrowed.
  const ctx = new OfflineAudioContext(1, 1, RATE)
  const decoded = await ctx.decodeAudioData(buffer)

  const channels: Float32Array[] = []
  for (let c = 0; c < decoded.numberOfChannels; c++) {
    // Copied out of the AudioBuffer so the arrays can be transferred to the
    // worker; a view into an AudioBuffer cannot be.
    channels.push(decoded.getChannelData(c).slice())
  }
  return { channels, duration: decoded.duration }
}
