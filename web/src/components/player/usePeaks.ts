import { useEffect, useState } from 'react'

import { decodeForWaveform } from './decode'
import type { PeaksFailure, PeaksResponse } from './peaks.worker'

export interface Peaks {
  values: Float32Array
  duration: number
}

/**
 * Computes the waveform for a file, in a worker.
 *
 * Returns undefined while it works, which the caller draws as a skeleton the
 * width of the waveform — the shape is known, so there is no reason for the
 * layout to move when the real thing arrives.
 */
export function usePeaks(file: File | undefined, buckets: number): {
  peaks: Peaks | undefined
  error: string | undefined
} {
  // The reset happens during render rather than in the effect: choosing a new
  // file must clear the old waveform before anything draws, and doing it in an
  // effect shows the previous file's peaks for a frame.
  const [state, setState] = useState<{
    file: File | undefined
    peaks: Peaks | undefined
    error: string | undefined
  }>({ file, peaks: undefined, error: undefined })

  if (state.file !== file) {
    setState({ file, peaks: undefined, error: undefined })
  }

  useEffect(() => {
    if (!file) return

    let cancelled = false
    const worker = new Worker(new URL('./peaks.worker.ts', import.meta.url), { type: 'module' })

    worker.onmessage = (event: MessageEvent<PeaksResponse | PeaksFailure>) => {
      if (cancelled) return
      setState(
        event.data.ok
          ? {
              file,
              peaks: { values: event.data.peaks, duration: event.data.duration },
              error: undefined,
            }
          : { file, peaks: undefined, error: event.data.error },
      )
      worker.terminate()
    }
    worker.onerror = (event) => {
      if (!cancelled) setState({ file, peaks: undefined, error: event.message })
      worker.terminate()
    }

    void file
      .arrayBuffer()
      .then(decodeForWaveform)
      .then(({ channels, duration }) => {
        if (cancelled) {
          worker.terminate()
          return
        }
        // Transferred, not copied: the worker is about to own these samples.
        worker.postMessage(
          { channels, buckets, duration },
          channels.map((c) => c.buffer),
        )
      })
      .catch((err: unknown) => {
        if (!cancelled) setState({ file, peaks: undefined, error: String(err) })
        worker.terminate()
      })

    return () => {
      cancelled = true
      worker.terminate()
    }
  }, [file, buckets])

  return { peaks: state.peaks, error: state.error }
}
