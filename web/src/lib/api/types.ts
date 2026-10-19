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
  /** What a person called the voice; only voice isolation names them. */
  name?: string
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
  /**
   * Null when VAD did not run — "nothing was measured", which is not the same
   * as "no silence was found". Readers have to tell the two apart.
   */
  silence: Silence[] | null
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
  /** Whether this model can be biased towards a hotword dictionary. */
  hotwords: boolean
  /** Why it cannot, in words a person can act on. Empty when it can. */
  hotwords_reason?: string
}

export interface ModelInfo {
  id: string
  revision: string
  display_name: string
  kind: string
  family: string
  /** Also serves the realtime API. It decodes uploaded files all the same. */
  streaming?: boolean
  languages: string[]
  /** What a multilingual model can be told to transcribe, besides "auto". */
  locales?: string[]
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

/**
 * A stored hotword dictionary.
 *
 * `phrases` is absent in a listing — a page of dictionaries is a page of names
 * — so `phrase_count` is the one to read there, and `matches` says which
 * phrases a search matched.
 */
export interface Dictionary {
  key: string
  name: string
  description?: string
  /** null in a listing — "not included here" — and a list, possibly empty, when one dictionary is fetched. */
  phrases: string[] | null
  phrase_count: number
  matches?: string[]
  score?: number
  created_at: string
  updated_at: string
}

/** What the server will do with a dictionary once a request names one. */
export interface HotwordPolicy {
  enabled: boolean
  default_score: number
  max_variants: number
  max_phrases: number
  /** Keys applied to every request, from postproc.hotwords.default_dictionaries. */
  default_dictionaries?: string[]
}

export interface DictionaryPage {
  data: Dictionary[]
  policy: HotwordPolicy
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
  /** Keys of stored dictionaries, merged with hotwords above by the server. */
  hotwords_dict?: string[]
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
  /**
   * The list screen does not need per-row transcripts — that is what the
   * detail screen fetches. Setting includeResult=false makes a page of
   * long jobs a fraction of its default payload.
   */
  includeResult?: boolean
}

/** A job that can still change is worth watching; one that cannot is not. */
export function isTerminal(status: JobStatus): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'canceled' || status === 'expired'
}

/** Every word in time order — what the player binary-searches to highlight. */
export function flatWords(result: Result): Word[] {
  return result.segments.flatMap((s) => s.words ?? [])
}

// --- voice isolation ---------------------------------------------------------
// Mirrors internal/core/isolation.go.

export type IsolationStatus =
  | 'queued'
  | 'analysing'
  | 'awaiting_selection'
  | 'extracting'
  | 'transcribing'
  | 'done'
  | 'failed'

export type AudioFormat = 'wav' | 'mp3'

export interface VoiceSample {
  /** "window": 3 s from `start`; "assembled": joined from the voice's clean pieces. */
  kind: 'window' | 'assembled'
  start?: number
  similarity: number
}

export interface IsolationVoice {
  speaker: string
  /** What a person called it; empty until somebody does. */
  name?: string
  total_speech: number
  turns: number
  sample?: VoiceSample
  /** Why the voice has no sample, when it has none. */
  reason?: string
}

export interface IsolationSelection {
  speaker: string
  sample_start?: number
}

/** One isolated voice: a track the length of the recording. */
export interface IsolationResult {
  speaker: string
  /** The file name it downloads under, without an extension. */
  name: string
  duration: number
  sample_rate: number
  /** Where the sample was taken from, when a person placed it. */
  sample_start?: number
  job_id?: string
}

export interface TranscriptJob {
  speaker: string
  job_id: string
  status: JobStatus
  percent?: number
}

/** Where the transcript by speaker is; the transcript itself is fetched apart. */
export interface IsolationTranscript {
  status: JobStatus
  model?: string
  language?: string
  jobs?: TranscriptJob[]
  /** A track was isolated again after the transcript was made. */
  stale?: boolean
  warnings?: Warning[]
  error?: JobError
  created_at: string
  finished_at?: string
}

export interface Isolation {
  id: string
  status: IsolationStatus
  stage?: string
  percent?: number
  position?: number
  /** The voice the running stage works on. */
  voice?: string
  filename?: string
  source: 'api' | 'ui'
  duration?: number
  /** Stored until deleted, across restarts. */
  keep: boolean
  voices?: IsolationVoice[]
  selected?: IsolationSelection
  /** The track of the voice chosen last. */
  result?: IsolationResult
  tracks?: IsolationResult[]
  transcript?: IsolationTranscript
  formats?: AudioFormat[]
  warnings?: Warning[]
  error?: JobError
  created_at: string
  /** Absent for a kept isolation. */
  expires_at?: string
}

export interface IsolationInfo {
  enabled: boolean
  formats?: AudioFormat[]
  /** Whether an isolation may be kept until it is deleted. */
  keep?: boolean
}

export interface IsolationUpdate {
  keep?: boolean
  names?: Record<string, string>
}

/** Work is queued or running: the isolation is worth watching. */
export function isolationBusy(status: IsolationStatus): boolean {
  return (
    status === 'queued' || status === 'analysing' || status === 'extracting' || status === 'transcribing'
  )
}
