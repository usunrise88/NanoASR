import { describe, expect, it } from 'vitest'

import { bytes, clock } from './format'

describe('clock', () => {
  it('formats under an hour as m:ss', () => {
    expect(clock(0)).toBe('0:00')
    expect(clock(9)).toBe('0:09')
    expect(clock(62)).toBe('1:02')
    expect(clock(599)).toBe('9:59')
  })

  it('grows to h:mm:ss past an hour', () => {
    expect(clock(3600)).toBe('1:00:00')
    expect(clock(3661)).toBe('1:01:01')
  })

  // Word timings are floats and a seek can land just before zero; a clock
  // reading "-1:59" would look like a bug in the transcript.
  it('never shows a negative or a non-number', () => {
    expect(clock(-5)).toBe('0:00')
    expect(clock(Number.NaN)).toBe('0:00')
    expect(clock(Number.POSITIVE_INFINITY)).toBe('0:00')
  })

  it('truncates rather than rounds up', () => {
    expect(clock(59.9)).toBe('0:59')
  })
})

describe('bytes', () => {
  it('scales to the unit that reads shortest', () => {
    expect(bytes(0)).toBe('0 B')
    expect(bytes(512)).toBe('512 B')
    expect(bytes(1024)).toBe('1.0 kB')
    expect(bytes(1536)).toBe('1.5 kB')
    expect(bytes(100 * 1024 * 1024)).toBe('100 MB')
    expect(bytes(4 * 1024 ** 3)).toBe('4.0 GB')
  })

  it('does not invent a size for nonsense', () => {
    expect(bytes(-1)).toBe('0 B')
    expect(bytes(Number.NaN)).toBe('0 B')
  })
})
