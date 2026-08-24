import { Field as BaseField } from '@base-ui/react/field'
import { Switch as BaseSwitch } from '@base-ui/react/switch'
import type { InputHTMLAttributes, ReactNode } from 'react'

import { cn } from '@/lib/cn'

/**
 * Form controls, wrapped once.
 *
 * The label/description/error arrangement is here rather than in each form so
 * that a validation message always appears in the same place, in the same tone,
 * next to the input that caused it — which is what the API's `param` field
 * exists to make possible.
 */

const control =
  'h-8 w-full rounded-[var(--radius-md)] border border-[var(--border-default)] ' +
  'bg-[var(--bg-canvas)] px-2.5 text-[13px] text-[var(--text-primary)] ' +
  'placeholder:text-[var(--text-muted)] ' +
  'focus:border-[var(--accent-solid)] focus:outline-none ' +
  'disabled:opacity-50'

export interface FieldProps {
  label: string
  description?: string | undefined
  error?: string | undefined
  children: ReactNode
}

export function Field({ label, description, error, children }: FieldProps) {
  return (
    <BaseField.Root className="flex flex-col gap-1.5">
      <BaseField.Label className="text-[13px] font-medium">{label}</BaseField.Label>
      {children}
      {description && !error && (
        <p className="text-[12px] text-[var(--text-muted)]">{description}</p>
      )}
      {error && <p className="text-[12px] text-[var(--danger-text)]">{error}</p>}
    </BaseField.Root>
  )
}

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn(control, className)} {...rest} />
}

export interface SelectProps {
  value: string
  onChange: (value: string) => void
  options: { value: string; label: string; disabled?: boolean | undefined }[]
  disabled?: boolean | undefined
  id?: string | undefined
}

/**
 * A native select.
 *
 * Base UI has a richer one, and this deliberately is not it: the options here
 * are short lists of plain strings, and a native select brings keyboard
 * behaviour, screen-reader support and mobile pickers that a custom listbox
 * would have to reimplement to draw a nicer arrow.
 */
export function Select({ value, onChange, options, disabled, id }: SelectProps) {
  return (
    <select
      id={id}
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
      className={cn(control, 'appearance-none pr-8')}
    >
      {options.map((o) => (
        <option key={o.value} value={o.value} disabled={o.disabled}>
          {o.label}
        </option>
      ))}
    </select>
  )
}

export interface SwitchProps {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
  description?: string | undefined
  disabled?: boolean | undefined
}

export function Switch({ checked, onChange, label, description, disabled }: SwitchProps) {
  return (
    <label className={cn('flex items-start gap-2.5', disabled && 'opacity-50')}>
      <BaseSwitch.Root
        checked={checked}
        disabled={disabled}
        onCheckedChange={onChange}
        className={cn(
          'mt-0.5 h-4 w-7 shrink-0 rounded-full border border-[var(--border-default)]',
          'data-[checked]:border-[var(--accent-solid)] data-[checked]:bg-[var(--accent-solid)]',
          'bg-[var(--bg-raised)]',
        )}
      >
        <BaseSwitch.Thumb
          className={cn(
            'block h-3 w-3 translate-x-0.5 rounded-full bg-white',
            'data-[checked]:translate-x-3.5',
          )}
        />
      </BaseSwitch.Root>
      <span className="min-w-0">
        <span className="block text-[13px]">{label}</span>
        {description && (
          <span className="block text-[12px] text-[var(--text-muted)]">{description}</span>
        )}
      </span>
    </label>
  )
}
