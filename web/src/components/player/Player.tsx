import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Pause, Play } from 'iconoir-react'

import { Inline, Stack } from '@/components/layout'
import { IconButton, Select } from '@/components/ui'
import type { Result, Word } from '@/lib/api/types'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'

import { Waveform } from './Waveform'
import { usePeaks, type Peaks } from './usePeaks'
import { nextWord, previousWord } from './wordIndex'

/** Columns in the waveform. Enough detail for a long file, cheap to draw. */
const BUCKETS = 2000

const SPEEDS = ['0.75', '1', '1.25', '1.5', '2']

export interface PlayerHandle {
  seek: (seconds: number) => void
}

/**
 * Transport, waveform and keyboard, over a local File or a URL the server
 * streams. A File is decoded here for its waveform; a URL comes with the
 * waveform the server drew, since decoding it here would mean downloading all
 * of it first — exactly what streaming is for avoiding.
 *
 * The audio element is the source of truth for time — not React state — and the
 * position is read on each animation frame while playing. That keeps the
 * playhead and the highlighted word exact without a state update per frame.
 */
export function Player({
  file,
  src,
  peaks: given,
  result,
  words,
  currentTime,
  onTime,
  onReady,
  startAt,
  autoPlay,
  lanes,
  onPlaying,
}: {
  file?: File | undefined
  src?: string | undefined
  // peaks is the waveform of src; undefined draws a placeholder until it comes.
  peaks?: Peaks | undefined
  result: Result
  words: Word[]
  currentTime: number
  onTime: (seconds: number) => void
  onReady: (handle: PlayerHandle) => void
  // startAt and autoPlay carry the position over when the same recording is
  // swapped for another take of it — the whole conversation for one voice's
  // track — so listening goes on where it was.
  startAt?: number | undefined
  autoPlay?: boolean | undefined
  // lanes go under the waveform: who speaks when.
  lanes?: ReactNode
  onPlaying?: ((playing: boolean) => void) | undefined
}) {
  const t = useT()
  const audio = useRef<HTMLAudioElement>(null)
  const [playing, setPlaying] = useState(false)
  const [speed, setSpeed] = useState('1')
  const local = src ? undefined : file
  const computed = usePeaks(local, BUCKETS)
  const peaks = src ? given : computed.peaks
  const error = src ? undefined : computed.error

  // One object URL per file, revoked when it changes: a leaked blob URL pins
  // the whole file in memory for the life of the tab.
  const [objectURL, setObjectURL] = useState(() => (local ? URL.createObjectURL(local) : undefined))
  const [tracked, setTracked] = useState(local)
  if (tracked !== local) {
    if (objectURL) URL.revokeObjectURL(objectURL)
    setObjectURL(local ? URL.createObjectURL(local) : undefined)
    setTracked(local)
  }
  useEffect(
    () => () => {
      if (objectURL) URL.revokeObjectURL(objectURL)
    },
    [objectURL],
  )
  const url = src ?? objectURL

  const seek = useCallback(
    (seconds: number) => {
      const el = audio.current
      if (!el) return
      el.currentTime = Math.max(0, seconds)
      onTime(el.currentTime)
    },
    [onTime],
  )

  useEffect(() => onReady({ seek }), [onReady, seek])

  // Reading the position on a frame callback rather than from timeupdate: the
  // element fires that about four times a second, which is visibly behind the
  // word being spoken.
  useEffect(() => {
    if (!playing) return
    let frame = 0
    const tick = () => {
      const el = audio.current
      if (el) onTime(el.currentTime)
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [playing, onTime])

  const toggle = useCallback(() => {
    const el = audio.current
    if (!el) return
    if (el.paused) void el.play()
    else el.pause()
  }, [])

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const target = e.target as HTMLElement | null
      // Typing in the search box must not scrub the audio.
      if (target && ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName)) return

      const el = audio.current
      if (!el) return

      switch (e.key) {
        case ' ':
          e.preventDefault()
          toggle()
          break
        case 'ArrowLeft': {
          e.preventDefault()
          if (e.shiftKey) {
            const i = previousWord(words, el.currentTime)
            seek(i === -1 ? 0 : (words[i]?.start ?? 0))
          } else {
            seek(el.currentTime - 5)
          }
          break
        }
        case 'ArrowRight': {
          e.preventDefault()
          if (e.shiftKey) {
            const i = nextWord(words, el.currentTime)
            if (i !== -1) seek(words[i]?.start ?? el.currentTime)
          } else {
            seek(el.currentTime + 5)
          }
          break
        }
        default:
          break
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [toggle, seek, words])

  const duration = result.duration || peaks?.duration || 0

  return (
    <Stack gap={3}>
      <Waveform
        peaks={peaks}
        duration={duration}
        silence={result.silence}
        currentTime={currentTime}
        onSeek={seek}
      />
      {lanes}

      {error && <p className="text-[12px] text-[var(--danger-text)]">{error}</p>}

      <Inline gap={3}>
        <IconButton label={playing ? t('result.pause') : t('result.play')} onClick={toggle}>
          {playing ? <Pause width={15} height={15} /> : <Play width={15} height={15} />}
        </IconButton>

        <span className="text-[12px] tabular-nums text-[var(--text-secondary)]">
          {clock(currentTime)} / {clock(duration)}
        </span>

        <span className="ml-auto flex items-center gap-2">
          <span className="text-[12px] text-[var(--text-muted)]">{t('result.speed')}</span>
          <span className="w-20">
            <Select
              value={speed}
              onChange={(v) => {
                setSpeed(v)
                if (audio.current) audio.current.playbackRate = Number(v)
              }}
              options={SPEEDS.map((s) => ({ value: s, label: `${s}×` }))}
            />
          </span>
        </span>
      </Inline>

      <audio
        ref={audio}
        src={url}
        preload="metadata"
        onPlay={() => {
          setPlaying(true)
          onPlaying?.(true)
        }}
        onPause={() => {
          setPlaying(false)
          onPlaying?.(false)
        }}
        onEnded={() => {
          setPlaying(false)
          onPlaying?.(false)
        }}
        onSeeked={(e) => onTime(e.currentTarget.currentTime)}
        onLoadedMetadata={(e) => {
          const el = e.currentTarget
          el.playbackRate = Number(speed)
          if (startAt) el.currentTime = startAt
          if (autoPlay) void el.play()
        }}
      />
    </Stack>
  )
}
