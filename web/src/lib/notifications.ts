import { useSyncExternalStore } from 'react'

/**
 * Notification history.
 *
 * Toasts auto-dismiss, which makes them useless as a record. Every toast is
 * also appended here so the bell menu in the header can show what happened
 * while the user was looking elsewhere (SPEC §13.5).
 */
export type NotificationLevel = 'info' | 'success' | 'warning' | 'error'

export interface NotificationRecord {
  id: string
  level: NotificationLevel
  title: string
  description?: string
  at: number
  read: boolean
}

const LIMIT = 100
const STORAGE_KEY = 'nanoasr.notifications'

let records: NotificationRecord[] = load()
const listeners = new Set<() => void>()

function load(): NotificationRecord[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? (JSON.parse(raw) as NotificationRecord[]) : []
  } catch {
    return []
  }
}

function persist(): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(records))
  } catch {
    // History is a convenience; losing it must never break the page.
  }
}

function emit(): void {
  persist()
  for (const l of listeners) l()
}

export function record(n: Omit<NotificationRecord, 'id' | 'at' | 'read'>): void {
  records = [
    { ...n, id: crypto.randomUUID(), at: Date.now(), read: false },
    ...records,
  ].slice(0, LIMIT)
  emit()
}

export function markAllRead(): void {
  records = records.map((r) => ({ ...r, read: true }))
  emit()
}

export function clearAll(): void {
  records = []
  emit()
}

export function useNotifications(): NotificationRecord[] {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => records,
    () => records,
  )
}

export function useUnreadCount(): number {
  return useNotifications().filter((n) => !n.read).length
}
