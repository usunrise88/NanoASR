import { apiKey, reportAuthRequired } from '@/lib/auth'

import type {
  DownloadProgress,
  Job,
  JobFilter,
  JobPage,
  ModelInfo,
  ResponseFormat,
  TranscribeOptions,
} from './types'

/** Every path this client talks to lives under here. */
export const BASE = '/api/v1'

/**
 * A refusal from the server, in the shape it actually sends.
 *
 * The native dialect answers RFC 9457 problem+json, so `code` is a stable
 * machine-readable identifier and `param` names the offending field. Carrying
 * both means a form can put the message next to the input that caused it
 * instead of raising a toast about the whole request.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly param: string | undefined
  readonly retryAfter: number | undefined

  constructor(init: {
    status: number
    code: string
    detail: string
    param?: string
    retryAfter?: number
  }) {
    super(init.detail)
    this.name = 'ApiError'
    this.status = init.status
    this.code = init.code
    this.param = init.param
    this.retryAfter = init.retryAfter
  }
}

export function authHeaders(): HeadersInit {
  const key = apiKey()
  return key ? { Authorization: `Bearer ${key}` } : {}
}

async function toError(response: Response): Promise<ApiError> {
  const retryAfterHeader = response.headers.get('Retry-After')
  const retryAfter = retryAfterHeader ? Number(retryAfterHeader) : undefined

  let code = 'internal'
  let detail = response.statusText || `HTTP ${response.status}`
  let param: string | undefined

  try {
    const body = (await response.json()) as {
      code?: string
      detail?: string
      title?: string
      param?: string
    }
    code = body.code ?? code
    detail = body.detail ?? body.title ?? detail
    param = body.param
  } catch {
    // Not every failure comes from our handlers: a proxy in front can answer
    // with HTML, and the status is still the useful part.
  }

  return new ApiError({
    status: response.status,
    code,
    detail,
    ...(param === undefined ? {} : { param }),
    ...(retryAfter === undefined || Number.isNaN(retryAfter) ? {} : { retryAfter }),
  })
}

/** The one place a request leaves the app. */
export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(BASE + path, {
    ...init,
    headers: { ...authHeaders(), ...(init.headers ?? {}) },
  })

  if (response.status === 401) {
    reportAuthRequired()
  }
  if (!response.ok) {
    throw await toError(response)
  }
  if (response.status === 204) {
    return undefined as T
  }
  return (await response.json()) as T
}

// --- jobs -------------------------------------------------------------------

export function submitJob(
  file: File,
  options: TranscribeOptions,
  signal?: AbortSignal,
): Promise<Job> {
  const form = new FormData()
  form.set('file', file)
  // `source` is the honest label of where the work came from; it shows up in
  // the history filters. It does not buy priority — that belongs to the key.
  form.set('source', 'ui')

  for (const [name, value] of Object.entries(options)) {
    if (value === undefined || value === '' || value === false) continue
    if (Array.isArray(value)) {
      for (const item of value) form.append(`${name}[]`, String(item))
      continue
    }
    form.set(name, String(value))
  }

  return request<Job>('/jobs', { method: 'POST', body: form, ...(signal ? { signal } : {}) })
}

export function getJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${encodeURIComponent(id)}`)
}

export function listJobs(filter: JobFilter = {}): Promise<JobPage> {
  const q = new URLSearchParams()
  for (const status of filter.status ?? []) q.append('status', status)
  if (filter.model) q.set('model', filter.model)
  if (filter.source) q.set('source', filter.source)
  if (filter.since) q.set('since', filter.since)
  if (filter.limit) q.set('limit', String(filter.limit))
  if (filter.cursor) q.set('cursor', filter.cursor)

  const query = q.toString()
  return request<JobPage>(`/jobs${query ? `?${query}` : ''}`)
}

export function cancelJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/**
 * Fetches a finished transcript in a text format.
 *
 * Rendered by the server, not here. The Go side keeps one subtitle renderer for
 * both dialects precisely so the timecode arithmetic exists in one place; a
 * second implementation in TypeScript would be a second place to fix it, and
 * only one of them would get fixed.
 */
export async function fetchTranscript(id: string, format: ResponseFormat): Promise<string> {
  const response = await fetch(
    `${BASE}/jobs/${encodeURIComponent(id)}?response_format=${format}`,
    { headers: authHeaders() },
  )
  if (response.status === 401) reportAuthRequired()
  if (!response.ok) throw await toError(response)
  return response.text()
}

// --- models -----------------------------------------------------------------

interface ModelList {
  data: ModelInfo[]
}

export async function listModels(): Promise<ModelInfo[]> {
  return (await request<ModelList>('/models')).data
}

export async function listCatalog(): Promise<ModelInfo[]> {
  return (await request<ModelList>('/catalog')).data
}

export function loadModel(id: string): Promise<ModelInfo> {
  return request<ModelInfo>(`/models/${encodeURIComponent(id)}/load`, { method: 'POST' })
}

export function unloadModel(id: string): Promise<ModelInfo> {
  return request<ModelInfo>(`/models/${encodeURIComponent(id)}/unload`, { method: 'POST' })
}

export function pinModel(id: string, pinned: boolean): Promise<ModelInfo> {
  return request<ModelInfo>(
    `/models/${encodeURIComponent(id)}/pin?pinned=${pinned ? 'true' : 'false'}`,
    { method: 'POST' },
  )
}

export function reloadModel(id: string, revision: string): Promise<ModelInfo> {
  return request<ModelInfo>(
    `/models/${encodeURIComponent(id)}/reload?revision=${encodeURIComponent(revision)}`,
    { method: 'POST' },
  )
}

export function downloadPath(id: string): string {
  return `${BASE}/models/${encodeURIComponent(id)}/download`
}

export function jobEventsPath(id: string): string {
  return `${BASE}/jobs/${encodeURIComponent(id)}/events`
}

export type { DownloadProgress }
