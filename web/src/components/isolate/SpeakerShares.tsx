import type { IsolationVoice } from '@/lib/api/types'
import { cn } from '@/lib/cn'
import type { SpeakerShare } from '@/lib/conversation'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'
import { speakerIndex, speakerTone } from '@/lib/speakers'

import { VoiceName } from './VoiceName'

/**
 * Who said how much, a card per voice. A card is also the voice's filter: it
 * hides and shows the voice's turns, and its name can be changed here, where
 * the reader has just found out who it is.
 */
export function SpeakerShares({
  shares,
  voices,
  hidden,
  onToggle,
  onRename,
}: {
  shares: SpeakerShare[]
  voices: IsolationVoice[] | undefined
  hidden: ReadonlySet<string>
  onToggle: (speaker: string) => void
  onRename?: ((speaker: string, name: string) => Promise<void>) | undefined
}) {
  const t = useT()
  return (
    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
      {shares.map((s) => {
        const index = speakerIndex(voices, s.speaker)
        const tone = speakerTone(index)
        const off = hidden.has(s.speaker)
        const percent = Math.round(s.share * 100)
        return (
          <div
            key={s.speaker}
            data-share={s.speaker}
            className={cn(
              'rounded-[var(--radius-md)] border px-3 py-2.5',
              off ? 'border-[var(--border-subtle)] opacity-60' : tone.border,
            )}
          >
            <div className="flex items-center justify-between gap-2">
              <VoiceName
                speaker={s.speaker}
                name={voices?.find((v) => v.speaker === s.speaker)?.name}
                index={index}
                onRename={onRename && ((name) => onRename(s.speaker, name))}
              />
              <button
                type="button"
                aria-pressed={!off}
                className="shrink-0 rounded-[var(--radius-sm)] px-1.5 py-0.5 text-[11px] text-[var(--text-secondary)] hover:bg-[var(--bg-hover)]"
                onClick={() => onToggle(s.speaker)}
              >
                {off ? t('isolate.show') : t('isolate.hide')}
              </button>
            </div>
            <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-[var(--bg-raised)]">
              <div
                className={cn('h-full', tone.solid)}
                // The width is the value, as in Progress: data, not styling.
                ref={(el) => {
                  if (el) el.style.width = `${percent}%`
                }}
              />
            </div>
            <div className="mt-1.5 flex flex-wrap gap-x-3 text-[12px] tabular-nums text-[var(--text-secondary)]">
              <span>{clock(s.seconds)}</span>
              <span>{percent}%</span>
              <span>{t('isolate.words', { count: s.words })}</span>
              <span>{t('isolate.turns', { count: s.turns })}</span>
            </div>
          </div>
        )
      })}
    </div>
  )
}
