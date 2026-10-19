import type { IsolationVoice } from '@/lib/api/types'

/**
 * A voice's colour, the same wherever it appears. Written out whole because
 * Tailwind finds classes by reading the source: a class assembled from a
 * number at runtime would never be generated.
 */
export interface SpeakerTone {
  /** Badges, timeline bars. */
  solid: string
  /** A turn's background. */
  soft: string
  text: string
  border: string
  /** The raw colour, for canvas and geometry in components/player. */
  color: string
}

const TONES: SpeakerTone[] = [
  {
    solid: 'bg-[var(--speaker-1-solid)]',
    soft: 'bg-[var(--speaker-1-bg)]',
    text: 'text-[var(--speaker-1-text)]',
    border: 'border-[var(--speaker-1-border)]',
    color: 'var(--speaker-1-solid)',
  },
  {
    solid: 'bg-[var(--speaker-2-solid)]',
    soft: 'bg-[var(--speaker-2-bg)]',
    text: 'text-[var(--speaker-2-text)]',
    border: 'border-[var(--speaker-2-border)]',
    color: 'var(--speaker-2-solid)',
  },
  {
    solid: 'bg-[var(--speaker-3-solid)]',
    soft: 'bg-[var(--speaker-3-bg)]',
    text: 'text-[var(--speaker-3-text)]',
    border: 'border-[var(--speaker-3-border)]',
    color: 'var(--speaker-3-solid)',
  },
  {
    solid: 'bg-[var(--speaker-4-solid)]',
    soft: 'bg-[var(--speaker-4-bg)]',
    text: 'text-[var(--speaker-4-text)]',
    border: 'border-[var(--speaker-4-border)]',
    color: 'var(--speaker-4-solid)',
  },
  {
    solid: 'bg-[var(--speaker-5-solid)]',
    soft: 'bg-[var(--speaker-5-bg)]',
    text: 'text-[var(--speaker-5-text)]',
    border: 'border-[var(--speaker-5-border)]',
    color: 'var(--speaker-5-solid)',
  },
  {
    solid: 'bg-[var(--speaker-6-solid)]',
    soft: 'bg-[var(--speaker-6-bg)]',
    text: 'text-[var(--speaker-6-text)]',
    border: 'border-[var(--speaker-6-border)]',
    color: 'var(--speaker-6-solid)',
  },
]

/** The voice's place among the voices found, which fixes its colour. */
export function speakerIndex(voices: IsolationVoice[] | undefined, speaker: string): number {
  const i = voices?.findIndex((v) => v.speaker === speaker) ?? -1
  if (i >= 0) return i
  // Not among the voices (a transcript read without them): its number.
  const n = /^spk_(\d+)$/.exec(speaker)
  return n ? Number(n[1]) : 0
}

export function speakerTone(index: number): SpeakerTone {
  return TONES[((index % TONES.length) + TONES.length) % TONES.length] as SpeakerTone
}
