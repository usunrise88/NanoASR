import { describe, expect, it } from 'vitest'

import { SseParser } from './sse'

describe('SseParser', () => {
  it('reads a complete frame', () => {
    const events = new SseParser().push('id: 3\nevent: running\ndata: {"id":"job_1"}\n\n')

    expect(events).toEqual([{ id: '3', event: 'running', data: '{"id":"job_1"}' }])
  })

  // Chunk boundaries fall wherever the network puts them. This is the reason
  // the parser holds a buffer instead of parsing each chunk on its own.
  it('joins a frame split across chunks', () => {
    const parser = new SseParser()

    expect(parser.push('id: 1\nevent: qu')).toEqual([])
    expect(parser.push('eued\ndata: {"a":')).toEqual([])
    expect(parser.push('1}\n\n')).toEqual([
      { id: '1', event: 'queued', data: '{"a":1}' },
    ])
  })

  it('reads several frames from one chunk', () => {
    const events = new SseParser().push(
      'id: 1\nevent: queued\ndata: 1\n\nid: 2\nevent: running\ndata: 2\n\n',
    )

    expect(events.map((e) => e.event)).toEqual(['queued', 'running'])
    expect(events.map((e) => e.id)).toEqual(['1', '2'])
  })

  it('keeps a trailing partial frame for the next chunk', () => {
    const parser = new SseParser()

    expect(parser.push('data: 1\n\ndata: 2')).toHaveLength(1)
    expect(parser.push('\n\n')).toEqual([{ id: '', event: 'message', data: '2' }])
  })

  // The server sends comment lines as heartbeats so an idle stream is not
  // closed by a proxy. They carry no data and must not surface as events.
  it('ignores heartbeat comments', () => {
    const parser = new SseParser()

    expect(parser.push(': keep-alive\n\n')).toEqual([])
    expect(parser.push(': keep-alive\n\ndata: 1\n\n')).toEqual([
      { id: '', event: 'message', data: '1' },
    ])
  })

  it('rejoins a multi-line data payload', () => {
    const events = new SseParser().push('data: first\ndata: second\n\n')

    expect(events[0]?.data).toBe('first\nsecond')
  })

  it('tolerates CRLF line endings', () => {
    const events = new SseParser().push('id: 7\r\nevent: running\r\ndata: {"a":1}\r\n\r\n')

    expect(events).toEqual([{ id: '7', event: 'running', data: '{"a":1}' }])
  })

  // The nastiest split there is: a CRLF pair torn in half by a chunk boundary.
  // Converting the lone CR to a newline when it arrives would invent a blank
  // line and end the frame one field early.
  it('joins a CRLF torn across chunks', () => {
    const parser = new SseParser()

    expect(parser.push('id: 7\r')).toEqual([])
    expect(parser.push('\nevent: running\r')).toEqual([])
    expect(parser.push('\ndata: {"a":1}\r\n\r\n')).toEqual([
      { id: '7', event: 'running', data: '{"a":1}' },
    ])
  })

  // A field with no value is legal, and "data:" with nothing after it is an
  // empty payload rather than an absent one.
  it('handles a field with no space after the colon', () => {
    const events = new SseParser().push('event:done\ndata:{}\n\n')

    expect(events).toEqual([{ id: '', event: 'done', data: '{}' }])
  })

  it('defaults the event name to message', () => {
    const events = new SseParser().push('data: 1\n\n')

    expect(events[0]?.event).toBe('message')
  })

  // A catch-up snapshot carries no id, so the client's Last-Event-ID stays
  // where it was rather than advancing past events that never existed.
  it('reports an absent id as empty', () => {
    const events = new SseParser().push('event: succeeded\ndata: {}\n\n')

    expect(events[0]?.id).toBe('')
  })
})
