import { useSyncExternalStore } from 'react'

/**
 * The API key the SPA sends, and whether the server wants one at all.
 *
 * There is no configuration flag saying "this server needs a key". The SPA
 * finds out the way any client does — by getting 401 — which is both simpler
 * than publishing the answer on a public endpoint and impossible to get out of
 * step with the server's actual behaviour.
 *
 * localStorage rather than memory: the UI and the API are one origin, and a key
 * the user has to retype on every reload is a key they will paste into
 * something worse.
 */
const STORAGE_KEY = 'nanoasr.apikey'

let key = load()
let required = false
const listeners = new Set<() => void>()

function load(): string {
  try {
    return localStorage.getItem(STORAGE_KEY) ?? ''
  } catch {
    return ''
  }
}

function emit(): void {
  for (const l of listeners) l()
}

export function apiKey(): string {
  return key
}

export function setApiKey(value: string): void {
  key = value.trim()
  try {
    if (key) localStorage.setItem(STORAGE_KEY, key)
    else localStorage.removeItem(STORAGE_KEY)
  } catch {
    // A key that survives only this session still works.
  }
  emit()
}

/**
 * Called by the client when the server answers 401. From then on the UI knows a
 * key is needed and can say so instead of showing empty screens.
 */
export function reportAuthRequired(): void {
  if (required && !key) return
  required = true
  emit()
}

export interface AuthState {
  key: string
  /** True once the server has refused an unauthenticated request. */
  required: boolean
}

let snapshot: AuthState = { key, required }

function currentSnapshot(): AuthState {
  if (snapshot.key !== key || snapshot.required !== required) {
    snapshot = { key, required }
  }
  return snapshot
}

export function useAuth(): AuthState {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    currentSnapshot,
    currentSnapshot,
  )
}
