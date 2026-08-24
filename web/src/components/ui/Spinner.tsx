import { cn } from '@/lib/cn'

/**
 * The single spinner primitive: thin, monochrome, inline.
 *
 * Spinners mark an action in flight — a submit, a model load. They never take
 * over the screen; unknown-shape waits use a skeleton instead (SPEC §13.4).
 *
 * The rotation is declared in motion.css against [data-spinner], like every
 * other piece of motion in the product. Keeping it here as an animate- class
 * would make this primitive the one exception to the rule it exists to enforce.
 */
export function Spinner({ className, label }: { className?: string; label?: string }) {
  return (
    <span
      role="status"
      aria-label={label ?? 'Loading'}
      data-spinner=""
      className={cn(
        'inline-block h-3.5 w-3.5 shrink-0 rounded-full border-[1.5px]',
        'border-[var(--border-strong)] border-t-transparent',
        className,
      )}
    />
  )
}
