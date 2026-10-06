import { Field as BaseField } from '@base-ui/react/field'
import { Switch as BaseSwitch } from '@base-ui/react/switch'
import { createContext, useContext, useId } from 'react'
import type { InputHTMLAttributes, ReactNode, TextareaHTMLAttributes } from 'react'

import { cn } from '@/lib/cn'

/**
 * The id a Field hands to the control inside it.
 *
 * Without it the label is a sibling of the input with nothing tying the two
 * together: a screen reader announces an unlabelled text box, and clicking the
 * label does nothing. Passing the id through context rather than asking every
 * call site to invent one keeps `<Field label=…><Input/></Field>` the whole of
 * what a form has to write.
 */
const FieldId = createContext<string | undefined>(undefined)

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
  const id = useId()
  return (
    <BaseField.Root className="flex flex-col gap-1.5">
      <BaseField.Label htmlFor={id} className="text-[13px] font-medium">
        {label}
      </BaseField.Label>
      <FieldId.Provider value={id}>{children}</FieldId.Provider>
      {description && !error && (
        <p className="text-[12px] text-[var(--text-muted)]">{description}</p>
      )}
      {error && <p className="text-[12px] text-[var(--danger-text)]">{error}</p>}
    </BaseField.Root>
  )
}

export function Input({ className, id, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  const fieldId = useContext(FieldId)
  return <input id={id ?? fieldId} className={cn(control, className)} {...rest} />
}

/**
 * A multi-line input, for the one thing in this product that is genuinely a
 * list: the phrases of a hotword dictionary. It borrows the control styling
 * rather than its height, which is the caller's to set.
 */
export function Textarea({ className, id, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  const fieldId = useContext(FieldId)
  return (
    <textarea
      id={id ?? fieldId}
      className={cn(control, 'h-auto py-2 leading-5', className)}
      {...rest}
    />
  )
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
  const fieldId = useContext(FieldId)
  return (
    <select
      id={id ?? fieldId}
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
