import { describe, expect, it } from 'vitest'

import { downmix, normalise, rmsEnvelope } from './peaks'

describe('rmsEnvelope', () => {
  it('splits the signal into the requested number of columns', () => {
    expect(rmsEnvelope(new Float32Array(1000), 200)).toHaveLength(200)
  })

  it('measures loudness per window', () => {
    // Silence, then full scale: two buckets, two very different values.
    const samples = new Float32Array([0, 0, 0, 0, 1, 1, 1, 1])
    const env = rmsEnvelope(samples, 2)

    expect(env[0]).toBe(0)
    expect(env[1]).toBeCloseTo(1)
  })

  // A single clipped sample makes a peak plot look like solid speech. RMS
  // follows loudness, which is the thing a reader is looking at.
  it('does not let one loud sample fill a window', () => {
    const spike = new Float32Array(100)
    spike[0] = 1
    const env = rmsEnvelope(spike, 1)

    expect(env[0]).toBeLessThan(0.2)
  })

  it('survives more columns than samples', () => {
    const env = rmsEnvelope(new Float32Array([1, 1]), 8)

    expect(env).toHaveLength(8)
    expect([...env].every((v) => Number.isFinite(v))).toBe(true)
  })

  it('returns an empty envelope for empty input', () => {
    expect(rmsEnvelope(new Float32Array(0), 10).every((v) => v === 0)).toBe(true)
    expect(rmsEnvelope(new Float32Array([1]), 0)).toHaveLength(0)
  })
})

describe('normalise', () => {
  // Telephony is recorded quiet; un-normalised it draws as a flat line with the
  // speech invisible inside it.
  it('lifts a quiet signal so the loudest column reaches 1', () => {
    const out = normalise(new Float32Array([0.01, 0.02, 0.04]))

    expect(out[2]).toBeCloseTo(1)
    expect(out[0]).toBeCloseTo(0.25)
  })

  it('leaves silence alone rather than dividing by zero', () => {
    const out = normalise(new Float32Array([0, 0, 0]))

    expect([...out]).toEqual([0, 0, 0])
  })
})

describe('downmix', () => {
  it('averages channels', () => {
    const out = downmix([new Float32Array([1, 0]), new Float32Array([0, 1])])

    expect([...out]).toEqual([0.5, 0.5])
  })

  it('passes mono through untouched', () => {
    const mono = new Float32Array([0.5])

    expect(downmix([mono])).toBe(mono)
  })

  it('handles no channels at all', () => {
    expect(downmix([])).toHaveLength(0)
  })
})
