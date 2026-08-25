import { describe, expect, it } from 'vitest'

import type { Word } from '@/lib/api/types'

import { nextWord, previousWord, wordAt } from './wordIndex'

const words: Word[] = [
  { word: 'один', start: 0.0, end: 0.5 },
  { word: 'два', start: 0.6, end: 1.0 },
  { word: 'три', start: 2.0, end: 2.4 },
]

describe('wordAt', () => {
  it('finds the word being spoken', () => {
    expect(wordAt(words, 0.2)).toBe(0)
    expect(wordAt(words, 0.8)).toBe(1)
    expect(wordAt(words, 2.1)).toBe(2)
  })

  it('includes both boundaries', () => {
    expect(wordAt(words, 0)).toBe(0)
    expect(wordAt(words, 0.5)).toBe(0)
    expect(wordAt(words, 2.4)).toBe(2)
  })

  // Most of a real recording is between words. Highlighting the neighbour
  // through every pause would make the transcript flicker constantly.
  it('highlights nothing in a gap', () => {
    expect(wordAt(words, 0.55)).toBe(-1)
    expect(wordAt(words, 1.5)).toBe(-1)
  })

  it('highlights nothing before the first or after the last word', () => {
    expect(wordAt(words, -1)).toBe(-1)
    expect(wordAt(words, 99)).toBe(-1)
  })

  it('handles an empty transcript', () => {
    expect(wordAt([], 1)).toBe(-1)
  })

  // Observed against a real recording: clicking a word seeks to its start, the
  // audio element quantises that to its sample grid, and the value comes back
  // forty-eight nanoseconds early. An exact comparison put the playhead in the
  // gap before the word and highlighted nothing — the one interaction this
  // screen exists for.
  it('still finds a word when the seek lands a hair short of its start', () => {
    const real: Word[] = [
      { word: 'счастлив', start: 1.7819999475479127, end: 2.2619998474121092 },
      { word: 'уж', start: 2.3020000476837157, end: 2.4219999332427977 },
      { word: 'я', start: 2.5019998569488524, end: 2.582000019073486 },
    ]

    expect(wordAt(real, 2.302)).toBe(1)
  })

  // The tolerance must not reach across a real gap: 80 ms between words is
  // ordinary, and highlighting the next word that early would be visible.
  it('does not stretch across the gap between words', () => {
    const real: Word[] = [
      { word: 'уж', start: 2.302, end: 2.422 },
      { word: 'я', start: 2.502, end: 2.582 },
    ]

    expect(wordAt(real, 2.45)).toBe(-1)
    expect(wordAt(real, 2.48)).toBe(-1)
  })
})

describe('nextWord and previousWord', () => {
  it('steps forward from anywhere, including a gap', () => {
    expect(nextWord(words, -1)).toBe(0)
    expect(nextWord(words, 0.2)).toBe(1)
    expect(nextWord(words, 1.5)).toBe(2)
  })

  // Stepping back from partway through a word goes to that word's own start,
  // the way skipping back a track does — only once you are already at the start
  // does it reach the previous one.
  it('steps back to the current word first, then to the previous', () => {
    expect(previousWord(words, 2.1)).toBe(2)
    expect(previousWord(words, 2.0)).toBe(1)
    expect(previousWord(words, 0.8)).toBe(1)
    expect(previousWord(words, 1.5)).toBe(1) // from a gap, the word before it
  })

  it('reports the ends rather than wrapping', () => {
    expect(nextWord(words, 99)).toBe(-1)
    expect(previousWord(words, -1)).toBe(-1)
  })

  // Sitting exactly on a word's start must step off it, not back onto it —
  // otherwise pressing "previous" twice in a row goes nowhere.
  it('does not stall on a word boundary', () => {
    expect(previousWord(words, 0.6)).toBe(0)
    expect(nextWord(words, 0.6)).toBe(2)
  })

  it('reaches every word by stepping repeatedly', () => {
    const visited: number[] = []
    let at = -1
    for (let i = nextWord(words, at); i !== -1; i = nextWord(words, at)) {
      visited.push(i)
      at = words[i]?.start ?? at
    }
    expect(visited).toEqual([0, 1, 2])
  })
})
