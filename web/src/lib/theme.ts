import { useSyncExternalStore } from 'react'

export type Theme = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'nanoasr.theme'
const listeners = new Set<() => void>()
let theme: Theme = read()

function read(): Theme {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // Ignore: the default follows the system.
  }
  return 'system'
}

function resolve(t: Theme): 'light' | 'dark' {
  if (t !== 'system') return t
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

/** Applies the theme by toggling the class Radix Colors keys its dark scale on. */
export function applyTheme(): void {
  document.documentElement.classList.toggle('dark', resolve(theme) === 'dark')
}

export function setTheme(next: Theme): void {
  theme = next
  try {
    localStorage.setItem(STORAGE_KEY, next)
  } catch {
    // Persisting the preference is optional.
  }
  applyTheme()
  for (const l of listeners) l()
}

export function useTheme(): Theme {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => theme,
    () => theme,
  )
}

/** Keeps `system` honest when the OS preference changes mid-session. */
export function watchSystemTheme(): () => void {
  const mq = window.matchMedia('(prefers-color-scheme: dark)')
  const onChange = () => {
    if (theme === 'system') applyTheme()
  }
  mq.addEventListener('change', onChange)
  return () => mq.removeEventListener('change', onChange)
}
