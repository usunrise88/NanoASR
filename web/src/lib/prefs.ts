import { useSyncExternalStore } from 'react'

/**
 * Display preferences that belong to this browser rather than to the server.
 *
 * The confidence threshold is the only one so far: how uncertain a word has to
 * be before the transcript marks it is a judgement about how much the reader
 * wants to be warned, not a property of the model.
 */
const STORAGE_KEY = 'nanoasr.prefs'

export interface Prefs {
  /** Words below this confidence are underlined. 0 marks nothing. */
  confidenceThreshold: number
}

const DEFAULTS: Prefs = { confidenceThreshold: 0 }

let prefs: Prefs = load()
const listeners = new Set<() => void>()

function load(): Prefs {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? { ...DEFAULTS, ...(JSON.parse(raw) as Partial<Prefs>) } : DEFAULTS
  } catch {
    return DEFAULTS
  }
}

export function setPrefs(next: Partial<Prefs>): void {
  prefs = { ...prefs, ...next }
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(prefs))
  } catch {
    // A preference that lasts only this session still works.
  }
  for (const l of listeners) l()
}

export function usePrefs(): Prefs {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => prefs,
    () => prefs,
  )
}
