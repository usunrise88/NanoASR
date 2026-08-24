import { beforeEach, describe, expect, it } from 'vitest'

import { forget, recall, remember } from './audio-store'

function file(name: string): File {
  return new File([new Uint8Array(4)], name)
}

describe('audio store', () => {
  beforeEach(() => {
    for (const id of ['a', 'b', 'c', 'd', 'e']) forget(id)
  })

  it('hands back the file a job was run on', () => {
    remember('a', file('one.wav'))
    expect(recall('a')?.name).toBe('one.wav')
    expect(recall('missing')).toBeUndefined()
  })

  // Twenty transcriptions in a session should not pin twenty files in memory.
  // Losing the oldest leaves that result exactly where a reload leaves it.
  it('keeps only the most recent few', () => {
    for (const id of ['a', 'b', 'c', 'd']) remember(id, file(`${id}.wav`))

    expect(recall('a')).toBeUndefined()
    expect(recall('d')?.name).toBe('d.wav')
  })

  it('re-remembering a job keeps it recent', () => {
    remember('a', file('a.wav'))
    remember('b', file('b.wav'))
    remember('c', file('c.wav'))
    remember('a', file('a2.wav')) // touched, so it is no longer the oldest
    remember('d', file('d.wav'))

    expect(recall('a')?.name).toBe('a2.wav')
    expect(recall('b')).toBeUndefined()
  })
})
