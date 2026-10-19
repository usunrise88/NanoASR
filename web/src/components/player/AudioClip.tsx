import { useCallback, useEffect, useRef, useState, type MouseEvent } from 'react'
import { Pause, Play } from 'iconoir-react'

import { IconButton } from '@/components/ui'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'

/**
 * A short clip with a play button, a seekable bar and a clock: a voice sample,
 * or the isolated voice beside the original.
 *
 * The audio is a Blob, or a function that fetches one: a voice sample is only
 * fetched when somebody presses play, so a recording with eight voices does not
 * download eight samples nobody listens to. Starting one clip stops the others
 * on the page — two voices at once is exactly what this screen removes.
 */
export function AudioClip({
  blob,
  load,
  label,
}: {
  blob?: Blob | undefined
  load?: (() => Promise<Blob>) | undefined
  label: string
}) {
  const t = useT()
  const audio = useRef<HTMLAudioElement>(null)
  const [playing, setPlaying] = useState(false)
  const [time, setTime] = useState(0)
  const [duration, setDuration] = useState(0)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string>()

  const [fetched, setFetched] = useState<Blob>()
  const source = blob ?? fetched

  // One object URL per blob, revoked when it changes: a leaked blob URL pins
  // the audio in memory for the life of the tab.
  const [url, setUrl] = useState(() => (source ? URL.createObjectURL(source) : undefined))
  const [tracked, setTracked] = useState(source)
  if (tracked !== source) {
    if (url) URL.revokeObjectURL(url)
    setUrl(source ? URL.createObjectURL(source) : undefined)
    setTracked(source)
  }
  useEffect(() => () => {
    if (url) URL.revokeObjectURL(url)
  }, [url])

  const pendingPlay = useRef(false)
  useEffect(() => {
    if (url && pendingPlay.current) {
      pendingPlay.current = false
      void audio.current?.play()
    }
  }, [url])

  const toggle = useCallback(async () => {
    const el = audio.current
    if (!el) return
    if (!url) {
      if (!load) return
      setLoading(true)
      setError(undefined)
      try {
        pendingPlay.current = true
        setFetched(await load())
      } catch (err) {
        pendingPlay.current = false
        setError(err instanceof Error ? err.message : String(err))
      } finally {
        setLoading(false)
      }
      return
    }
    if (el.paused) void el.play()
    else el.pause()
  }, [url, load])

  useEffect(() => {
    const el = audio.current
    if (!el) return
    const onPlay = () => {
      setPlaying(true)
      document.dispatchEvent(new CustomEvent('nanoasr:clip-play', { detail: el }))
    }
    const onOther = (e: Event) => {
      if ((e as CustomEvent<HTMLAudioElement>).detail !== el) el.pause()
    }
    const onPause = () => setPlaying(false)
    const onTime = () => setTime(el.currentTime)
    const onMeta = () => setDuration(Number.isFinite(el.duration) ? el.duration : 0)
    el.addEventListener('play', onPlay)
    el.addEventListener('pause', onPause)
    el.addEventListener('ended', onPause)
    el.addEventListener('timeupdate', onTime)
    el.addEventListener('loadedmetadata', onMeta)
    document.addEventListener('nanoasr:clip-play', onOther)
    return () => {
      el.removeEventListener('play', onPlay)
      el.removeEventListener('pause', onPause)
      el.removeEventListener('ended', onPause)
      el.removeEventListener('timeupdate', onTime)
      el.removeEventListener('loadedmetadata', onMeta)
      document.removeEventListener('nanoasr:clip-play', onOther)
    }
  }, [])

  function seek(e: MouseEvent<HTMLDivElement>) {
    const el = audio.current
    if (!el || !duration) return
    const box = e.currentTarget.getBoundingClientRect()
    el.currentTime = Math.max(0, Math.min(duration, ((e.clientX - box.left) / box.width) * duration))
    setTime(el.currentTime)
  }

  const percent = duration ? Math.min(100, (time / duration) * 100) : 0

  return (
    <div className="flex min-w-0 items-center gap-2" data-clip={label}>
      <audio ref={audio} src={url} preload="metadata" />
      <IconButton
        label={playing ? t('isolate.pause') : t('isolate.play')}
        busy={loading}
        onClick={() => void toggle()}
      >
        {playing ? <Pause width={14} height={14} /> : <Play width={14} height={14} />}
      </IconButton>
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <div className="flex items-baseline justify-between gap-2">
          <span className="truncate text-[12px] text-[var(--text-secondary)]">{label}</span>
          <span className="shrink-0 text-[11px] tabular-nums text-[var(--text-muted)]">
            {clock(time)}
            {duration > 0 && ` / ${clock(duration)}`}
          </span>
        </div>
        <div
          role="slider"
          tabIndex={-1}
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={Math.round(duration)}
          aria-valuenow={Math.round(time)}
          onClick={seek}
          className="h-1 cursor-pointer overflow-hidden rounded-full bg-[var(--bg-raised)]"
        >
          <div className="h-full bg-[var(--accent-solid)]" style={{ width: `${percent}%` }} />
        </div>
        {error && <span className="text-[11px] text-[var(--danger-text)]">{error}</span>}
      </div>
    </div>
  )
}
