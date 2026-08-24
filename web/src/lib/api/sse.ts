import { authHeaders } from './client'

/**
 * Server-sent events read with fetch rather than EventSource.
 *
 * EventSource cannot set an Authorization header, and the only way around that
 * is a token in the query string — which lands in the server's access log the
 * moment it is used. fetch also hands us the Last-Event-ID on reconnect instead
 * of managing it invisibly, which is what makes resuming a stream something the
 * caller can reason about.
 */
export interface SseEvent {
  id: string
  event: string
  data: string
}

/**
 * Splits a byte stream into events.
 *
 * Kept as a class with a `push` method because chunk boundaries fall wherever
 * the network puts them: a frame regularly arrives in two pieces, and a parser
 * that assumes each chunk is whole loses every event that straddles one.
 */
export class SseParser {
  private buffer = ''
  /** A lone CR held back from the previous chunk; see push. */
  private pendingCR = false

  push(chunk: string): SseEvent[] {
    // Line endings are normalised to LF before anything looks for a frame
    // boundary: with CRLF the terminator is \r\n\r\n, which a scan for \n\n
    // never finds — the events simply never arrive. Our server sends LF, but
    // the protocol allows CRLF and an intermediary may rewrite them.
    //
    // A trailing CR is held back rather than converted, because it may be the
    // first half of a CRLF whose LF is in the next chunk; converting it now
    // would invent a blank line and split a frame in half.
    let text = (this.pendingCR ? '\r' : '') + chunk
    this.pendingCR = text.endsWith('\r')
    if (this.pendingCR) text = text.slice(0, -1)
    this.buffer += text.replace(/\r\n/g, '\n').replace(/\r/g, '\n')

    const events: SseEvent[] = []

    // A blank line terminates an event. Anything after the last one is a
    // partial frame and stays in the buffer for the next chunk.
    let boundary = this.buffer.indexOf('\n\n')
    while (boundary !== -1) {
      const frame = this.buffer.slice(0, boundary)
      this.buffer = this.buffer.slice(boundary + 2)
      const parsed = parseFrame(frame)
      if (parsed) events.push(parsed)
      boundary = this.buffer.indexOf('\n\n')
    }
    return events
  }
}

function parseFrame(frame: string): SseEvent | null {
  let id = ''
  let event = 'message'
  const data: string[] = []

  for (const line of frame.split('\n')) {
    // A line starting with a colon is a comment; the server sends them as
    // heartbeats so idle streams are not closed by a proxy.
    if (line === '' || line.startsWith(':')) continue

    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    switch (field) {
      case 'id':
        id = value
        break
      case 'event':
        event = value
        break
      case 'data':
        data.push(value)
        break
      default:
        break
    }
  }

  if (data.length === 0) return null
  return { id, event, data: data.join('\n') }
}

export interface StreamOptions<T> {
  /** Called for each event whose payload parses. */
  onEvent: (event: string, payload: T, id: string) => void
  /** Called once the server says the work is over, or the stream cannot resume. */
  onDone?: () => void
  onError?: (error: unknown) => void
  signal?: AbortSignal
  /** How many times to reconnect before giving up. */
  maxRetries?: number
}

const RETRY_DELAY_MS = [500, 2_000, 5_000]

/**
 * Reads an SSE endpoint to completion, resuming where it left off.
 *
 * The server ends a finished stream with an explicit `done` event rather than
 * by closing quietly, so this knows the difference between "the work is over"
 * and "the connection dropped" — and only reconnects for the second.
 */
export async function stream<T>(path: string, opts: StreamOptions<T>): Promise<void> {
  const maxRetries = opts.maxRetries ?? RETRY_DELAY_MS.length
  let lastEventId = ''
  let attempt = 0

  for (;;) {
    let finished: boolean
    try {
      finished = await readOnce(path, lastEventId, opts, (id) => {
        if (id) lastEventId = id
      })
    } catch (err) {
      if (opts.signal?.aborted) return
      if (attempt >= maxRetries) {
        opts.onError?.(err)
        opts.onDone?.()
        return
      }
      await delay(RETRY_DELAY_MS[Math.min(attempt, RETRY_DELAY_MS.length - 1)] ?? 5_000, opts.signal)
      attempt++
      continue
    }

    if (finished || opts.signal?.aborted) {
      opts.onDone?.()
      return
    }
    // The stream ended without saying the work was done: the connection went
    // away, so resume from the last event we actually saw.
    if (attempt >= maxRetries) {
      opts.onDone?.()
      return
    }
    await delay(RETRY_DELAY_MS[Math.min(attempt, RETRY_DELAY_MS.length - 1)] ?? 5_000, opts.signal)
    attempt++
  }
}

/** Reads one connection. Returns true if the server said the work is over. */
async function readOnce<T>(
  path: string,
  lastEventId: string,
  opts: StreamOptions<T>,
  remember: (id: string) => void,
): Promise<boolean> {
  const headers: Record<string, string> = {
    ...(authHeaders() as Record<string, string>),
    Accept: 'text/event-stream',
  }
  if (lastEventId) headers['Last-Event-ID'] = lastEventId

  const response = await fetch(path, {
    headers,
    ...(opts.signal ? { signal: opts.signal } : {}),
  })
  if (!response.ok || !response.body) {
    throw new Error(`stream ${path}: HTTP ${response.status}`)
  }

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader()
  const parser = new SseParser()

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) return false
      for (const frame of parser.push(value)) {
        remember(frame.id)
        if (frame.event === 'done') return true

        let payload: T
        try {
          payload = JSON.parse(frame.data) as T
        } catch {
          continue // a frame we cannot read is not a reason to drop the stream
        }
        opts.onEvent(frame.event, payload, frame.id)
      }
    }
  } finally {
    await reader.cancel().catch(() => {})
  }
}

function delay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const id = window.setTimeout(resolve, ms)
    signal?.addEventListener('abort', () => {
      window.clearTimeout(id)
      resolve()
    }, { once: true })
  })
}
