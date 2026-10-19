import { describe, expect, it } from 'vitest'

import type { Result, Segment } from '@/lib/api/types'

import { conversation, currentWords, initials, shares, voiceName } from './conversation'

function seg(id: number, speaker: string, start: number, end: number, ...words: string[]): Segment {
  const step = (end - start) / Math.max(1, words.length)
  return {
    id,
    start,
    end,
    text: words.join(' '),
    channel: 0,
    speaker,
    words: words.map((w, i) => ({ word: w, start: start + i * step, end: start + (i + 1) * step })),
  }
}

function result(segments: Segment[]): Result {
  return {
    id: 'iso_1',
    model: 'm',
    language: 'ru',
    duration: 30,
    text: '',
    timestamp_source: 'token',
    segments,
    silence: [],
    speakers: [
      { id: 'spk_0', total_speech: 6, segments: 2 },
      { id: 'spk_1', total_speech: 2, segments: 1 },
    ],
    stats: { audio_duration: 30, processing_ms: 0, rtf: 0, stages_ms: {}, segments_total: 0, speech_ratio: 0 },
  }
}

describe('conversation', () => {
  const r = result([
    seg(0, 'spk_0', 0, 2, 'привет', 'как'),
    seg(1, 'spk_0', 2.5, 4, 'дела'),
    // Starts while spk_0 is still speaking.
    seg(2, 'spk_1', 3.5, 5.5, 'хорошо'),
    seg(3, 'spk_0', 10, 12, 'отлично'),
  ])
  const c = conversation(r)

  it('joins a speaker’s segments into turns and keeps an overlap apart', () => {
    expect(c.turns.map((t) => [t.speaker, t.segments.length, t.overlaps])).toEqual([
      ['spk_0', 2, false],
      ['spk_1', 1, true],
      ['spk_0', 1, false],
    ])
    expect(c.turns[0]?.end).toBe(4)
    // Word numbers run through the conversation in segment order.
    expect(c.turns[2]?.segments[0]?.first).toBe(4)
  })

  it('highlights a word per voice, both where they overlap', () => {
    expect(currentWords(c.bySpeaker, 0.5)).toEqual([0])
    expect(currentWords(c.bySpeaker, 3.8).sort()).toEqual([2, 3])
    // A pause turns the highlight off.
    expect(currentWords(c.bySpeaker, 8)).toEqual([])
    expect(c.words.map((w) => w.word)).toEqual(['привет', 'как', 'дела', 'хорошо', 'отлично'])
  })

  it('counts each voice’s share', () => {
    const s = shares(r, c.turns)
    expect(s.map((x) => [x.speaker, x.words, x.turns, x.share])).toEqual([
      ['spk_0', 4, 2, 0.75],
      ['spk_1', 1, 1, 0.25],
    ])
  })
})

describe('names', () => {
  it('uses the name a person gave, else the id', () => {
    const voices = [
      { speaker: 'spk_0', name: 'Анна', total_speech: 1, turns: 1 },
      { speaker: 'spk_1', total_speech: 1, turns: 1 },
    ]
    expect(voiceName(voices, 'spk_0')).toBe('Анна')
    expect(voiceName(voices, 'spk_1')).toBe('spk_1')
  })

  it('makes a badge of initials or of the number', () => {
    expect(initials('Анна Каренина', 0)).toBe('АК')
    expect(initials('борис', 1)).toBe('Б')
    expect(initials('spk_3', 2)).toBe('3')
  })
})
