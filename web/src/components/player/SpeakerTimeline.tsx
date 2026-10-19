import { memo } from 'react'

import type { IsolationVoice, Result } from '@/lib/api/types'
import { cn } from '@/lib/cn'
import { voiceName } from '@/lib/conversation'
import { speakerIndex, speakerTone } from '@/lib/speakers'

export interface Lane {
  speaker: string
  label: string
  /** A CSS colour: the voice's token. */
  color: string
  spans: { start: number; end: number }[]
}

/**
 * A lane per speaker of a result, each segment of theirs a bar. voices names
 * and orders them where an isolation found them; a diarized job has none,
 * and its speakers go by their ids.
 */
export function speakerLanes(result: Result, voices?: IsolationVoice[]): Lane[] {
  return (result.speakers ?? []).map((s) => ({
    speaker: s.id,
    label: voiceName(voices, s.id),
    color: speakerTone(speakerIndex(voices, s.id)).color,
    spans: result.segments.filter((seg) => seg.speaker === s.id).map((seg) => ({ start: seg.start, end: seg.end })),
  }))
}

/**
 * Who speaks when: a lane per voice under the waveform, each turn a bar in
 * the voice's colour. Overlapping speech is two bars stacked one above the
 * other, which is the one thing a single waveform cannot show. The lanes are
 * exactly as wide as the waveform, so a bar sits under the sound it is; the
 * voice is told by its colour, which its name carries everywhere else.
 *
 * The bars are positioned in percent of the recording, so the lanes stretch
 * with the page without measuring it; the playhead is the only part that
 * moves, and it is a sibling of the memoised bars so following it does not
 * redraw them.
 */
export function SpeakerTimeline({
  lanes,
  duration,
  currentTime,
  onSeek,
  dimmed,
}: {
  lanes: Lane[]
  duration: number
  currentTime: number
  onSeek: (seconds: number) => void
  // dimmed voices are drawn faded: the ones a filter hides.
  dimmed?: (speaker: string) => boolean
}) {
  const total = duration || 1
  const at = Math.min(100, Math.max(0, (currentTime / total) * 100))

  return (
    <div className="flex flex-col gap-1" data-timeline>
      {lanes.map((lane) => (
        <div key={lane.speaker} title={lane.label} data-lane={lane.speaker}>
          <div
            role="presentation"
            className="relative h-2.5 cursor-pointer rounded-[3px] bg-[var(--bg-raised)]"
            onClick={(e) => {
              const box = e.currentTarget.getBoundingClientRect()
              onSeek(((e.clientX - box.left) / box.width) * total)
            }}
          >
            <Bars lane={lane} total={total} faded={dimmed?.(lane.speaker) ?? false} />
            <span
              className="pointer-events-none absolute top-[-2px] bottom-[-2px] w-px bg-[var(--text-primary)]"
              style={{ left: `${at}%` }}
            />
          </div>
        </div>
      ))}
    </div>
  )
}

const Bars = memo(function Bars({ lane, total, faded }: { lane: Lane; total: number; faded: boolean }) {
  return (
    <>
      {lane.spans.map((s, i) => (
        <span
          key={i}
          className={cn('absolute top-0 bottom-0 rounded-[2px]', faded && 'opacity-25')}
          style={{
            left: `${(s.start / total) * 100}%`,
            width: `${Math.max(0.15, ((s.end - s.start) / total) * 100)}%`,
            background: lane.color,
          }}
        />
      ))}
    </>
  )
})
