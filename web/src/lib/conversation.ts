import type { IsolationVoice, Result, Segment, Word } from '@/lib/api/types'

/**
 * A transcript by speaker, read as a conversation.
 *
 * The server's segments are VAD stretches of one track each, sorted by start;
 * a person reads turns. A turn is a speaker's consecutive segments with no one
 * else starting in between and no long pause. Two voices that overlap stay two
 * turns, one after the other, and the second is marked as spoken over the
 * first — that is the case this transcript exists for.
 */

/** A pause longer than this starts a new turn even for the same speaker. */
const TURN_GAP = 3

export interface TurnSegment {
  segment: Segment
  /** Where the segment's words begin in the conversation's word numbering. */
  first: number
}

export interface Turn {
  key: string
  speaker: string
  start: number
  end: number
  segments: TurnSegment[]
  /** Starts while another voice is still speaking. */
  overlaps: boolean
}

export interface TimedWord {
  start: number
  end: number
  /** The word's number, as the DOM is told it (data-word). */
  index: number
}

export interface Conversation {
  turns: Turn[]
  /** Every word in time order, for the player's word-by-word keys. */
  words: Word[]
  /** Each speaker's words in time order: at most one is current per voice. */
  bySpeaker: Map<string, TimedWord[]>
}

export function conversation(result: Result): Conversation {
  const turns: Turn[] = []
  const bySpeaker = new Map<string, TimedWord[]>()
  const words: Word[] = []
  let n = 0
  // The latest end of anyone else's speech, to tell an overlap.
  const ends = new Map<string, number>()

  for (const segment of result.segments) {
    const speaker = segment.speaker ?? ''
    const last = turns[turns.length - 1]
    const first = n
    const own = bySpeaker.get(speaker) ?? []
    for (const w of segment.words ?? []) {
      own.push({ start: w.start, end: w.end, index: n })
      words.push(w)
      n++
    }
    bySpeaker.set(speaker, own)

    if (last && last.speaker === speaker && segment.start - last.end < TURN_GAP) {
      last.segments.push({ segment, first })
      last.end = Math.max(last.end, segment.end)
    } else {
      let overlaps = false
      for (const [other, end] of ends) {
        if (other !== speaker && end > segment.start + 0.2) overlaps = true
      }
      turns.push({
        key: `${speaker}-${segment.id}`,
        speaker,
        start: segment.start,
        end: segment.end,
        segments: [{ segment, first }],
        overlaps,
      })
    }
    ends.set(speaker, Math.max(ends.get(speaker) ?? 0, segment.end))
  }
  for (const own of bySpeaker.values()) own.sort((a, b) => a.start - b.start)
  words.sort((a, b) => a.start - b.start)
  return { turns, words, bySpeaker }
}

/**
 * The words being spoken at time t, one per voice at most: in overlapping
 * speech both are highlighted, which is how the reader sees it is overlap.
 */
export function currentWords(bySpeaker: Map<string, TimedWord[]>, t: number): number[] {
  const out: number[] = []
  for (const words of bySpeaker.values()) {
    let lo = 0
    let hi = words.length - 1
    let found = -1
    while (lo <= hi) {
      const mid = (lo + hi) >> 1
      const w = words[mid]
      if (!w) break
      if (w.start <= t) {
        found = mid
        lo = mid + 1
      } else {
        hi = mid - 1
      }
    }
    const w = found >= 0 ? words[found] : undefined
    // A word stays current through the short gap after it, so the highlight
    // does not flicker between words; a pause turns it off.
    if (w && t <= w.end + 0.3) out.push(w.index)
  }
  return out
}

/** How much each voice says, for the summary beside the transcript. */
export interface SpeakerShare {
  speaker: string
  seconds: number
  words: number
  turns: number
  /** Of everyone's speech together, 0–1. */
  share: number
}

export function shares(result: Result, turns: Turn[]): SpeakerShare[] {
  const out = new Map<string, SpeakerShare>()
  for (const s of result.speakers ?? []) {
    out.set(s.id, { speaker: s.id, seconds: s.total_speech, words: 0, turns: 0, share: 0 })
  }
  for (const segment of result.segments) {
    const row = out.get(segment.speaker ?? '')
    if (row) row.words += (segment.words ?? []).length || segment.text.split(/\s+/).filter(Boolean).length
  }
  for (const turn of turns) {
    const row = out.get(turn.speaker)
    if (row) row.turns++
  }
  const total = [...out.values()].reduce((sum, r) => sum + r.seconds, 0)
  for (const row of out.values()) row.share = total > 0 ? row.seconds / total : 0
  return [...out.values()]
}

/** Who a voice is to a reader: the name a person gave it, or its id. */
export function voiceName(voices: IsolationVoice[] | undefined, speaker: string): string {
  return voices?.find((v) => v.speaker === speaker)?.name || speaker
}

/** The letters on a voice's badge: initials of its name, or its number. */
export function initials(name: string, index: number): string {
  if (/^spk_\d+$/.test(name)) return String(index + 1)
  const parts = name.trim().split(/\s+/).filter(Boolean)
  const letters = parts.slice(0, 2).map((p) => [...p][0] ?? '')
  return letters.join('').toUpperCase() || String(index + 1)
}
