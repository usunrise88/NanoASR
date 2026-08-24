import type { TKey } from './i18n'
import type { JobStatus, ModelState } from './api/types'

/**
 * Formatting shared by every screen.
 *
 * A duration or a status shown two different ways in two places reads as two
 * different things, so each has exactly one implementation.
 */

/** mm:ss, or h:mm:ss past an hour. The player and the history agree on this. */
export function clock(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) seconds = 0
  const total = Math.floor(seconds)
  const s = total % 60
  const m = Math.floor(total / 60) % 60
  const h = Math.floor(total / 3600)
  const mm = h > 0 ? String(m).padStart(2, '0') : String(m)
  return h > 0
    ? `${h}:${mm}:${String(s).padStart(2, '0')}`
    : `${mm}:${String(s).padStart(2, '0')}`
}

const UNITS = ['B', 'kB', 'MB', 'GB', 'TB']

export function bytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const i = Math.min(UNITS.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  const value = n / 1024 ** i
  return `${value < 10 && i > 0 ? value.toFixed(1) : Math.round(value)} ${UNITS[i]}`
}

/** A time a person can read at a glance, in their own locale. */
export function when(iso: string | undefined, locale: string): string {
  if (!iso) return '—'
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return '—'

  const elapsed = Date.now() - date.getTime()
  if (elapsed < 60_000) return 'now'
  if (elapsed < 86_400_000) {
    return date.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit' })
  }
  return date.toLocaleDateString(locale, { day: 'numeric', month: 'short' })
}

export function statusKey(status: JobStatus): TKey {
  return `status.${status}` as TKey
}

export function modelStateKey(state: ModelState): TKey {
  return `status.${state}` as TKey
}

/** The tone a status is shown in, so a failure looks like one everywhere. */
export function statusTone(status: JobStatus): 'neutral' | 'accent' | 'success' | 'danger' {
  switch (status) {
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'danger'
    case 'running':
      return 'accent'
    default:
      return 'neutral'
  }
}

export function modelTone(state: ModelState): 'neutral' | 'accent' | 'success' {
  switch (state) {
    case 'ready':
      return 'success'
    case 'loading':
    case 'downloading':
      return 'accent'
    default:
      return 'neutral'
  }
}
