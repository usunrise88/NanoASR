/**
 * The wire shapes of the native dialect (`/api/v1`).
 *
 * Written by hand rather than generated: there are a dozen structs, they change
 * with the Go types they mirror, and a generator would add a build step whose
 * output nobody reads. The Go definitions are in internal/core/types.go and
 * internal/core/service.go; keep the two in step.
 */

export type JobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled' | 'expired'

export type TimestampSource = 'token' | 'segment' | 'aligned'

export type ChannelMode = 'downmix' | 'first' | 'split'

export type ModelState = 'absent' | 'downloading' | 'downloaded' | 'loading' | 'ready' | 'draining'

export interface Word {
  word: string
  start: number
  end: number
  confidence?: number
  original?: string
  speaker?: string | null
  speaker_confidence?: number
  channel?: number
}

export interface Segment {
  id: number
  start: number
  end: number
  text: string
  channel: number
  speaker: string | null
  avg_confidence?: number
  words?: Word[]
}

export interface Silence {
  start: number
  end: number
}

export interface Speaker {
  id: string
  total_speech: number
  segments: number
}

export interface Stats {
  audio_duration: number
  processing_ms: number
  rtf: number
  stages_ms: Record<string, number>
  segments_total: number
  speech_ratio: number
}

export interface Warning {
  code: string
  message: string
}

export interface Result {
  id: string
  model: string
  language: string
  duration: number
  text: string
  timestamp_source: TimestampSource
  segments: Segment[]
  silence: Silence[]
  speakers?: Speaker[]
  stats: Stats
  warnings?: Warning[]
}

export interface JobError {
  code: string
  message: string
  param?: string
}

export interface Job {
  id: string
  status: JobStatus
  position?: number
  stage?: string
  percent?: number
  model_id: string
  model_rev: string
  filename?: string
  source: 'api' | 'ui'
  created_at: string
  started_at?: string
  finished_at?: string
  result?: Result
  error?: JobError
}

export interface JobPage {
  data: Job[]
  next_cursor?: string
}

export interface Capabilities {
  word_timestamps: boolean
  confidence: boolean
  language_detect: boolean
  punctuation_builtin: boolean
}

export interface ModelInfo {
  id: string
  revision: string
  display_name: string
  kind: string
  family: string
  languages: string[]
  license: string
  state: ModelState
  pinned: boolean
  ref_count: number
  rss_mb: number
  last_used_unix?: number
  capabilities: Capabilities
}

export interface DownloadProgress {
  model_id: string
  downloaded: number
  total: number
  percent: number
  done: boolean
  error?: string
}

/** Parameters accepted by POST /api/v1/jobs. */
export interface TranscribeOptions {
  model?: string
  language?: string
  channel_mode?: ChannelMode
  decoding_method?: 'greedy_search' | 'modified_beam_search'
  max_active_paths?: number
  diarize?: boolean
  num_speakers?: number
  punctuate?: boolean
  itn?: boolean
  hotwords?: string[]
  hotwords_score?: number
  strict?: boolean
}

/** Response formats a finished job can be fetched as. */
export type ResponseFormat = 'json' | 'text' | 'srt' | 'vtt'

/** Filters accepted by GET /api/v1/jobs. */
export interface JobFilter {
  status?: JobStatus[]
  model?: string
  source?: 'api' | 'ui'
  since?: string
  limit?: number
  cursor?: string
}

/** A job that can still change is worth watching; one that cannot is not. */
export function isTerminal(status: JobStatus): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'canceled' || status === 'expired'
}

/** Every word in time order — what the player binary-searches to highlight. */
export function flatWords(result: Result): Word[] {
  return result.segments.flatMap((s) => s.words ?? [])
}
