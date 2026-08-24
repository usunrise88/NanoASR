import type { ButtonHTMLAttributes, ReactNode } from 'react'

import { cn } from '@/lib/cn'

import { Spinner } from './Spinner'

/**
 * The one button.
 *
 * Variants are a closed set for the same reason spacing is: a page that needs a
 * shape not listed here is a page inventing a new visual language, and the
 * right fix is to add it once, not to hand-roll it locally.
 */
export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
export type ButtonSize = 'sm' | 'md'

const base =
  'inline-flex shrink-0 items-center justify-center gap-1.5 rounded-[var(--radius-md)] ' +
  'font-medium whitespace-nowrap select-none ' +
  'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-1 ' +
  'focus-visible:outline-[var(--accent-solid)] ' +
  'disabled:pointer-events-none disabled:opacity-50'

const variantClass: Record<ButtonVariant, string> = {
  primary:
    'bg-[var(--accent-solid)] text-white hover:bg-[var(--accent-solid-hover)]',
  secondary:
    'border border-[var(--border-default)] bg-[var(--bg-surface)] text-[var(--text-primary)] ' +
    'hover:bg-[var(--bg-hover)]',
  ghost: 'text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] hover:text-[var(--text-primary)]',
  danger: 'bg-[var(--danger-solid)] text-white hover:opacity-90',
}

const sizeClass: Record<ButtonSize, string> = {
  sm: 'h-7 px-2.5 text-[12px]',
  md: 'h-8 px-3 text-[13px]',
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant | undefined
  size?: ButtonSize | undefined
  /** Replaces the leading icon with a spinner and disables the button. */
  busy?: boolean | undefined
  icon?: ReactNode | undefined
}

export function Button({
  variant = 'secondary',
  size = 'md',
  busy = false,
  icon,
  className,
  children,
  disabled,
  type = 'button',
  ...rest
}: ButtonProps) {
  return (
    <button
      type={type}
      disabled={disabled ?? busy}
      className={cn(base, variantClass[variant], sizeClass[size], className)}
      {...rest}
    >
      {/* The spinner replaces the icon rather than joining it, so the button
          keeps its width and the row does not jump when an action starts. */}
      {busy ? <Spinner className="h-3 w-3" /> : icon}
      {children}
    </button>
  )
}

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  /** Required: an icon alone tells a screen reader nothing. */
  label: string
  variant?: ButtonVariant | undefined
  busy?: boolean | undefined
}

export function IconButton({
  label,
  variant = 'ghost',
  busy = false,
  className,
  children,
  disabled,
  type = 'button',
  ...rest
}: IconButtonProps) {
  return (
    <button
      type={type}
      aria-label={label}
      title={label}
      disabled={disabled ?? busy}
      className={cn(base, variantClass[variant], 'h-7 w-7 p-0', className)}
      {...rest}
    >
      {busy ? <Spinner className="h-3 w-3" /> : children}
    </button>
  )
}
