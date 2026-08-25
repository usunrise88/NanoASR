import type { Word } from '@/lib/api/types'

/**
 * How far outside a word still counts as inside it.
 *
 * Seeking to a word's own start regularly lands just short of it: the audio
 * element quantises currentTime to its sample grid, so asking for 2.3020000476
 * gives back 2.302 — forty-eight nanoseconds early, enough for an exact
 * comparison to decide the playhead is in the gap before the word and highlight
 * nothing. The word you clicked would not light up.
 *
 * Ten milliseconds is far larger than that noise and far smaller than any real
 * gap between words, which run to tens of milliseconds even in fast speech.
 */
const TOLERANCE = 0.01

/**
 * Which word is being spoken at a given moment.
 *
 * Binary search, because this runs on every animation frame: a linear scan over
 * a ten-minute transcript is a few thousand comparisons sixty times a second,
 * and the result is used to toggle one CSS class.
 *
 * Returns -1 when the playhead is in a gap between words, which is most of the
 * silence in a real recording and must not highlight the neighbouring word.
 */
export function wordAt(words: Word[], time: number): number {
  let lo = 0
  let hi = words.length - 1
  let candidate = -1

  // Find the last word that starts at or before `time`.
  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    const word = words[mid]
    if (!word) break
    if (word.start <= time + TOLERANCE) {
      candidate = mid
      lo = mid + 1
    } else {
      hi = mid - 1
    }
  }

  if (candidate === -1) return -1
  const word = words[candidate]
  return word && time <= word.end + TOLERANCE ? candidate : -1
}

/** The next word after `time`, for keyboard navigation. -1 past the end. */
export function nextWord(words: Word[], time: number): number {
  let lo = 0
  let hi = words.length - 1
  let found = -1

  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    const word = words[mid]
    if (!word) break
    if (word.start > time + 1e-3) {
      found = mid
      hi = mid - 1
    } else {
      lo = mid + 1
    }
  }
  return found
}

/**
 * The word to step back to. -1 before the start.
 *
 * From partway through a word this is that word itself, so the seek lands on
 * its start — the same thing skipping back a track does. Only from a word's own
 * start does it reach the one before.
 */
export function previousWord(words: Word[], time: number): number {
  let lo = 0
  let hi = words.length - 1
  let found = -1

  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    const word = words[mid]
    if (!word) break
    if (word.start < time - 1e-3) {
      found = mid
      lo = mid + 1
    } else {
      hi = mid - 1
    }
  }
  return found
}
