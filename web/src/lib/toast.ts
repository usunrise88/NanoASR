import { toast as sonner } from 'sonner'

import { record, type NotificationLevel } from './notifications'

/**
 * The only way to raise a toast.
 *
 * Going through here guarantees two things a direct sonner call cannot: the
 * message lands in the notification history, and critical information is never
 * delivered by a surface that disappears on its own.
 */
interface ToastOptions {
  description?: string
  /** Milliseconds; ignored for `error`, which stays until dismissed. */
  duration?: number
}

function show(level: NotificationLevel, title: string, opts: ToastOptions = {}): void {
  record(
    opts.description === undefined
      ? { level, title }
      : { level, title, description: opts.description },
  )

  const args = {
    description: opts.description,
    duration: level === 'error' ? Infinity : (opts.duration ?? 4000),
  }
  switch (level) {
    case 'success':
      sonner.success(title, args)
      break
    case 'warning':
      sonner.warning(title, args)
      break
    case 'error':
      sonner.error(title, args)
      break
    default:
      sonner(title, args)
  }
}

export const toast = {
  info: (title: string, opts?: ToastOptions) => show('info', title, opts),
  success: (title: string, opts?: ToastOptions) => show('success', title, opts),
  warning: (title: string, opts?: ToastOptions) => show('warning', title, opts),
  error: (title: string, opts?: ToastOptions) => show('error', title, opts),
}
