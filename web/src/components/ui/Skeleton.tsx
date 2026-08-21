import { cn } from '@/lib/cn'

/**
 * The single skeleton primitive (SPEC §13.4).
 *
 * Skeletons are for data of known shape, sized to the real content so nothing
 * shifts when it arrives. The muted tone and the soft pulse come from
 * motion.css, which honours prefers-reduced-motion.
 */
export function Skeleton({ className }: { className?: string }) {
  return (
    <div
      data-skeleton=""
      aria-hidden="true"
      className={cn('rounded-[var(--radius-md)] bg-[var(--bg-raised)]', className)}
    />
  )
}

/** A skeleton shaped like a row in a list of models or jobs. */
export function SkeletonRow() {
  return (
    <div className="flex items-center gap-3 py-3">
      <Skeleton className="h-8 w-8 rounded-full" />
      <div className="flex-1">
        <Skeleton className="h-3.5 w-40" />
        <Skeleton className="mt-2 h-3 w-64" />
      </div>
      <Skeleton className="h-6 w-16" />
    </div>
  )
}
