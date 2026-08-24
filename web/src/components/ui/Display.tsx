import type { ReactNode } from 'react'

import { cn } from '@/lib/cn'

/** Small status displays, shared so a state looks the same wherever it appears. */

export type Tone = 'neutral' | 'accent' | 'success' | 'warning' | 'danger'

const toneClass: Record<Tone, string> = {
  neutral: 'bg-[var(--bg-raised)] text-[var(--text-secondary)]',
  accent: 'bg-[var(--accent-bg)] text-[var(--accent-text)]',
  success: 'bg-[var(--success-bg)] text-[var(--success-text)]',
  warning: 'bg-[var(--warning-bg)] text-[var(--warning-text)]',
  danger: 'bg-[var(--danger-bg)] text-[var(--danger-text)]',
}

export function Badge({
  tone = 'neutral',
  children,
  className,
}: {
  tone?: Tone
  children: ReactNode
  className?: string
}) {
  return (
    <span
      className={cn(
        'inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium whitespace-nowrap',
        toneClass[tone],
        className,
      )}
    >
      {children}
    </span>
  )
}

/**
 * A determinate progress bar.
 *
 * Determinate only: an indeterminate bar says "something is happening", which
 * is what the Spinner is for. This is used where a real fraction exists — a
 * download, a job's stages — and inventing one would be worse than not
 * showing a bar.
 */
export function Progress({ percent, label }: { percent: number; label?: string }) {
  const clamped = Math.max(0, Math.min(100, Math.round(percent)))
  return (
    <div className="flex items-center gap-2">
      <div
        role="progressbar"
        aria-valuenow={clamped}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={label ?? 'Progress'}
        className="h-1 flex-1 overflow-hidden rounded-full bg-[var(--bg-raised)]"
      >
        <div
          className="h-full bg-[var(--accent-solid)]"
          data-progress-bar=""
          // The width is the value: it is data, not styling, and there is no
          // token scale that could express "43 per cent".
          ref={(el) => {
            if (el) el.style.width = `${clamped}%`
          }}
        />
      </div>
      <span className="w-9 shrink-0 text-right text-[11px] tabular-nums text-[var(--text-muted)]">
        {clamped}%
      </span>
    </div>
  )
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string
  description?: string
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-12 text-center">
      <p className="text-[13px] font-medium">{title}</p>
      {description && (
        <p className="max-w-sm text-[12px] text-[var(--text-secondary)]">{description}</p>
      )}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}

export function ErrorState({
  title,
  detail,
  action,
}: {
  title: string
  detail?: string
  action?: ReactNode
}) {
  return (
    <div
      role="alert"
      className="rounded-[var(--radius-lg)] border border-[var(--danger-border)] bg-[var(--danger-bg)] p-3"
    >
      <p className="text-[13px] font-medium text-[var(--danger-text)]">{title}</p>
      {detail && <p className="mt-1 text-[12px] text-[var(--danger-text)] opacity-90">{detail}</p>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}

/** A definition row: label on the left, value on the right, aligned everywhere. */
export function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4 py-1">
      <span className="text-[12px] text-[var(--text-muted)]">{label}</span>
      <span className="text-[12px] tabular-nums">{children}</span>
    </div>
  )
}
