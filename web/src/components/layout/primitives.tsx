import type { ReactNode } from 'react'

import { cn } from '@/lib/cn'

/**
 * Layout primitives are the only way to build a page (SPEC §13.2).
 *
 * Spacing is a closed set of scale steps, expressed as a literal union: a page
 * cannot invent `gap: 13px`, because the type will not let it. Routes are
 * forbidden by ESLint from hand-rolling layout divs, so this file is where
 * every spacing decision in the product lives.
 */
export type Space = 1 | 2 | 3 | 4 | 6 | 8

const gapClass: Record<Space, string> = {
  1: 'gap-1',
  2: 'gap-2',
  3: 'gap-3',
  4: 'gap-4',
  6: 'gap-6',
  8: 'gap-8',
}

interface WithChildren {
  children: ReactNode
  className?: string
}

/** Page is the content column. The root shell renders the header above it. */
export function Page({ children, className }: WithChildren) {
  return (
    <div className={cn('mx-auto w-full max-w-5xl px-6 pb-16', className)}>{children}</div>
  )
}

/** Section groups related content and owns the rhythm between groups. */
export function Section({
  title,
  description,
  actions,
  children,
}: WithChildren & { title?: string; description?: string; actions?: ReactNode }) {
  return (
    <section className="mt-8 first:mt-0">
      {(title ?? actions) && (
        <div className="mb-3 flex items-baseline justify-between">
          <div>
            {title && <h2 className="text-[15px] font-medium">{title}</h2>}
            {description && (
              <p className="mt-0.5 text-[13px] text-[var(--text-secondary)]">{description}</p>
            )}
          </div>
          {actions}
        </div>
      )}
      {children}
    </section>
  )
}

/** Stack lays children out vertically with one of the allowed gaps. */
export function Stack({ children, gap = 4, className }: WithChildren & { gap?: Space }) {
  return <div className={cn('flex flex-col', gapClass[gap], className)}>{children}</div>
}

/** Inline lays children out horizontally, wrapping when it must. */
export function Inline({
  children,
  gap = 2,
  align = 'center',
  className,
}: WithChildren & { gap?: Space; align?: 'start' | 'center' | 'baseline' }) {
  return (
    <div
      className={cn(
        'flex flex-wrap',
        gapClass[gap],
        align === 'start' && 'items-start',
        align === 'center' && 'items-center',
        align === 'baseline' && 'items-baseline',
        className,
      )}
    >
      {children}
    </div>
  )
}

/** Grid is a responsive equal-column grid. */
export function Grid({ children, gap = 4, min = '18rem', className }: WithChildren & { gap?: Space; min?: string }) {
  return (
    <div
      className={cn('grid', gapClass[gap], className)}
      // Tailwind cannot express an arbitrary minmax track from a prop; this is
      // the single place in the app where a computed grid template is set.
      data-grid-min={min}
    >
      {children}
    </div>
  )
}

/** Card is the standard raised container. */
export function Card({ children, className }: WithChildren) {
  return (
    <div
      className={cn(
        'rounded-[var(--radius-lg)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] p-4',
        className,
      )}
    >
      {children}
    </div>
  )
}
