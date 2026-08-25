import { useEffect, useRef } from 'react'

import type { Silence } from '@/lib/api/types'
import { Skeleton } from '@/components/ui'

import type { Peaks } from './usePeaks'

/**
 * The waveform, the silence bands and the playhead, on one canvas.
 *
 * This is the one component allowed to compute pixel geometry — that is real
 * layout maths, not styling drift, and ESLint exempts this directory for it.
 *
 * The playhead is drawn rather than positioned with a DOM element so that
 * following it costs one canvas repaint per frame instead of a React render.
 */
export function Waveform({
  peaks,
  duration,
  silence,
  currentTime,
  onSeek,
  height = 72,
}: {
  peaks: Peaks | undefined
  duration: number
  silence: Silence[]
  currentTime: number
  onSeek: (seconds: number) => void
  height?: number
}) {
  const canvas = useRef<HTMLCanvasElement>(null)
  const box = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = canvas.current
    const parent = box.current
    if (!el || !parent || !peaks) return

    const draw = () => {
      const width = parent.clientWidth
      const dpr = window.devicePixelRatio || 1
      if (el.width !== width * dpr || el.height !== height * dpr) {
        el.width = width * dpr
        el.height = height * dpr
      }

      const ctx = el.getContext('2d')
      if (!ctx) return
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
      ctx.clearRect(0, 0, width, height)

      const styles = getComputedStyle(parent)
      const total = duration || peaks.duration || 1
      const perSecond = width / total

      // Silence first, as a band behind the wave: it is context for the shape
      // on top of it, not a mark of its own.
      ctx.fillStyle = styles.getPropertyValue('--wave-silence').trim()
      for (const gap of silence) {
        const x = gap.start * perSecond
        ctx.fillRect(x, 0, Math.max(1, (gap.end - gap.start) * perSecond), height)
      }

      const mid = height / 2
      const columns = peaks.values.length
      const columnWidth = width / columns

      ctx.fillStyle = styles.getPropertyValue('--wave-fill').trim()
      for (let i = 0; i < columns; i++) {
        const amplitude = (peaks.values[i] ?? 0) * (height / 2 - 2)
        ctx.fillRect(
          i * columnWidth,
          mid - amplitude,
          Math.max(0.5, columnWidth - 0.5),
          Math.max(1, amplitude * 2),
        )
      }

      const x = Math.min(width, Math.max(0, currentTime * perSecond))
      ctx.fillStyle = styles.getPropertyValue('--wave-played').trim()
      ctx.globalCompositeOperation = 'source-atop'
      ctx.fillRect(0, 0, x, height)
      ctx.globalCompositeOperation = 'source-over'

      ctx.fillStyle = styles.getPropertyValue('--wave-head').trim()
      ctx.fillRect(x - 0.5, 0, 1.5, height)
    }

    draw()
    const observer = new ResizeObserver(draw)
    observer.observe(parent)
    return () => observer.disconnect()
  }, [peaks, duration, silence, currentTime, height])

  function seekFromEvent(clientX: number) {
    const parent = box.current
    if (!parent) return
    const rect = parent.getBoundingClientRect()
    const ratio = Math.min(1, Math.max(0, (clientX - rect.left) / rect.width))
    onSeek(ratio * (duration || peaks?.duration || 0))
  }

  // Sized to the real waveform, so the layout does not move when it arrives.
  if (!peaks) {
    return <Skeleton className="h-[72px] w-full" />
  }

  return (
    <div
      ref={box}
      data-waveform=""
      role="slider"
      tabIndex={0}
      aria-label="Seek"
      aria-valuemin={0}
      aria-valuemax={Math.round(duration)}
      aria-valuenow={Math.round(currentTime)}
      className="relative h-[72px] w-full cursor-pointer select-none"
      onClick={(e) => seekFromEvent(e.clientX)}
      onKeyDown={(e) => {
        if (e.key === 'ArrowLeft') onSeek(Math.max(0, currentTime - 5))
        if (e.key === 'ArrowRight') onSeek(Math.min(duration, currentTime + 5))
      }}
    >
      <canvas ref={canvas} className="block h-full w-full" />
    </div>
  )
}
