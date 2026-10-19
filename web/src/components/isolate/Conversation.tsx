import { memo, useEffect, useRef } from 'react'

import type { IsolationVoice } from '@/lib/api/types'
import { cn } from '@/lib/cn'
import { currentWords, voiceName, type Conversation as Talk, type Turn } from '@/lib/conversation'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'
import { speakerIndex, speakerTone } from '@/lib/speakers'

import { VoiceBadge } from './VoiceName'

/**
 * The transcript by speaker as a conversation: a turn per speaker in their
 * colour, the words as seek targets.
 *
 * The words being spoken are marked through the DOM, not by rendering, as
 * the result page's transcript does: the turns are memoised and the time
 * changes sixty times a second. Two voices speaking at once each have their
 * word marked.
 */
export function Conversation({
  talk,
  voices,
  time,
  onSeek,
  query,
  hidden,
}: {
  talk: Talk
  voices: IsolationVoice[] | undefined
  time: number
  onSeek: (seconds: number) => void
  query: string
  hidden: ReadonlySet<string>
}) {
  const box = useRef<HTMLDivElement>(null)
  const marked = useRef<number[]>([])

  useEffect(() => {
    const root = box.current
    if (!root) return
    const now = currentWords(talk.bySpeaker, time)
    if (now.length === marked.current.length && now.every((w, i) => w === marked.current[i])) return
    for (const el of root.querySelectorAll('[data-current]')) el.removeAttribute('data-current')
    for (const i of now) {
      const el = root.querySelector<HTMLElement>(`[data-word="${i}"]`)
      el?.setAttribute('data-current', '')
    }
    // Follow the earliest of them; the other, if any, is a line away.
    if (now.length > 0) {
      root.querySelector<HTMLElement>(`[data-word="${Math.min(...now)}"]`)?.scrollIntoView({ block: 'nearest' })
    }
    marked.current = now
  }, [talk, time])

  return (
    <div ref={box}>
      <Turns talk={talk} voices={voices} onSeek={onSeek} query={query.trim().toLowerCase()} hidden={hidden} />
    </div>
  )
}

const Turns = memo(function Turns({
  talk,
  voices,
  onSeek,
  query,
  hidden,
}: {
  talk: Talk
  voices: IsolationVoice[] | undefined
  onSeek: (seconds: number) => void
  query: string
  hidden: ReadonlySet<string>
}) {
  const shown = talk.turns.filter((turn) => !hidden.has(turn.speaker))
  return (
    <div className="flex flex-col gap-3" data-conversation>
      {shown.map((turn) => (
        <TurnView key={turn.key} turn={turn} voices={voices} onSeek={onSeek} query={query} />
      ))}
    </div>
  )
})

function TurnView({
  turn,
  voices,
  onSeek,
  query,
}: {
  turn: Turn
  voices: IsolationVoice[] | undefined
  onSeek: (seconds: number) => void
  query: string
}) {
  const t = useT()
  const index = speakerIndex(voices, turn.speaker)
  const tone = speakerTone(index)
  const name = voiceName(voices, turn.speaker)

  return (
    <div className="flex gap-3" data-turn={turn.speaker}>
      <VoiceBadge name={name} index={index} />
      <div
        className={cn(
          'min-w-0 flex-1 rounded-[var(--radius-md)] border-l-2 px-3 py-2',
          tone.border,
          turn.overlaps ? tone.soft : 'bg-[var(--bg-surface)]',
        )}
      >
        <div className="mb-1 flex flex-wrap items-baseline gap-2">
          <span className={cn('text-[13px] font-medium', tone.text)}>{name}</span>
          <button
            type="button"
            className="text-[11px] tabular-nums text-[var(--text-muted)] hover:text-[var(--text-primary)]"
            onClick={() => onSeek(turn.start)}
          >
            {clock(turn.start)}
          </button>
          {turn.overlaps && (
            <span className="text-[11px] text-[var(--text-secondary)]">{t('isolate.overlap')}</span>
          )}
        </div>
        <p className="text-[14px] leading-relaxed">
          {turn.segments.map(({ segment, first }) =>
            (segment.words ?? []).length === 0 ? (
              <span key={segment.id}>{segment.text} </span>
            ) : (
              (segment.words ?? []).map((word, i) => (
                <span
                  key={`${segment.id}-${i}`}
                  role="button"
                  tabIndex={-1}
                  data-word={first + i}
                  onClick={() => onSeek(word.start)}
                  title={clock(word.start)}
                  className={cn(
                    'cursor-pointer rounded-[3px] px-px hover:bg-[var(--bg-hover)]',
                    'data-[current]:bg-[var(--accent-solid)] data-[current]:text-white',
                    query !== '' && word.word.toLowerCase().includes(query) && 'bg-[var(--warning-bg)]',
                  )}
                >
                  {word.word}{' '}
                </span>
              ))
            ),
          )}
        </p>
      </div>
    </div>
  )
}
