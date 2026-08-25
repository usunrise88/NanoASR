import { useEffect, useMemo, useRef } from 'react'

import type { Result, Word } from '@/lib/api/types'
import { cn } from '@/lib/cn'

import { wordAt } from './wordIndex'

/**
 * The transcript, with every word a seek target.
 *
 * The current word is highlighted by toggling a class on two DOM nodes per
 * frame through refs, not by re-rendering. A ten-minute transcript is a few
 * thousand spans; re-rendering that sixty times a second would make the page
 * unusable while playing, which is exactly when it matters most.
 */
export function Transcript({
  result,
  words,
  currentTime,
  onSeek,
  confidenceThreshold,
  query,
}: {
  result: Result
  words: Word[]
  currentTime: number
  onSeek: (seconds: number) => void
  confidenceThreshold: number
  query: string
}) {
  const container = useRef<HTMLDivElement>(null)
  const active = useRef(-1)

  useEffect(() => {
    const index = wordAt(words, currentTime)
    if (index === active.current) return

    const root = container.current
    if (!root) return

    root.querySelector('[data-current]')?.removeAttribute('data-current')
    if (index >= 0) {
      const el = root.querySelector<HTMLElement>(`[data-word="${index}"]`)
      el?.setAttribute('data-current', '')
      el?.scrollIntoView({ block: 'nearest' })
    }
    active.current = index
  }, [words, currentTime])

  const needle = query.trim().toLowerCase()

  // Where each segment's words begin in the flat array the player searches.
  // Computed once rather than accumulated while rendering: a counter mutated
  // inside the map would give different answers on a re-render that starts
  // partway through.
  const offsets = useMemo(() => {
    const out: number[] = []
    let n = 0
    for (const segment of result.segments) {
      out.push(n)
      n += (segment.words ?? []).length
    }
    return out
  }, [result.segments])

  return (
    <div ref={container} className="flex flex-col gap-3 leading-relaxed">
      {result.segments.map((segment, segmentIndex) => {
        const segmentWords = segment.words ?? []
        const first = offsets[segmentIndex] ?? 0

        return (
          <p key={segment.id} className="text-[14px]">
            {segmentWords.length === 0
              ? segment.text
              : segmentWords.map((word, i) => (
                  <span
                    key={`${segment.id}-${i}`}
                    role="button"
                    tabIndex={-1}
                    data-word={first + i}
                    onClick={() => onSeek(word.start)}
                    title={`${word.start.toFixed(2)}s`}
                    className={cn(
                      'cursor-pointer rounded-[3px] px-px',
                      'hover:bg-[var(--bg-hover)]',
                      'data-[current]:bg-[var(--accent-solid)] data-[current]:text-white',
                      // A word the model was unsure of is marked, not hidden:
                      // the reader decides whether to trust it.
                      confidenceThreshold > 0 &&
                        word.confidence !== undefined &&
                        word.confidence > 0 &&
                        word.confidence < confidenceThreshold &&
                        'underline decoration-dotted underline-offset-4',
                      needle !== '' &&
                        word.word.toLowerCase().includes(needle) &&
                        'bg-[var(--warning-bg)]',
                    )}
                  >
                    {word.word}{' '}
                  </span>
                ))}
          </p>
        )
      })}
    </div>
  )
}
