import { Dialog as BaseDialog } from '@base-ui/react/dialog'
import { Popover as BasePopover } from '@base-ui/react/popover'
import { Xmark } from 'iconoir-react'
import type { ReactNode } from 'react'

import { cn } from '@/lib/cn'

import { IconButton } from './Button'

/**
 * The surfaces, in order of how much they interrupt (SPEC §13.5).
 *
 * inline → popover → sheet → centre modal → toast
 *
 * Everything shares one backdrop, one motion opt-in and one focus behaviour
 * because they are all built here. A component that reached for Base UI itself
 * would fork all three, which is why ESLint forbids it outside this directory.
 */

const panel =
  'rounded-[var(--radius-lg)] border border-[var(--border-subtle)] ' +
  'bg-[var(--bg-surface)] shadow-lg outline-none'

/** A soft, not-black backdrop: the page behind stays legible. */
const backdrop = 'fixed inset-0 bg-[var(--slate-a8)]'

export interface SurfaceProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  children: ReactNode
  footer?: ReactNode
}

/**
 * A centre modal — for true interruptions only.
 *
 * If the user could reasonably keep working while this is open, it should have
 * been a Sheet. The rule is enforced by there being exactly one Dialog in the
 * product and a reviewer able to see every use of it.
 */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
}: SurfaceProps) {
  return (
    <BaseDialog.Root open={open} onOpenChange={onOpenChange}>
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className={backdrop} data-motion="enter" />
        <BaseDialog.Popup
          data-motion="enter"
          className={cn(
            panel,
            'fixed top-1/2 left-1/2 z-50 w-[min(28rem,calc(100vw-2rem))]',
            '-translate-x-1/2 -translate-y-1/2 p-4',
          )}
        >
          <SurfaceHeader title={title} description={description} onClose={() => onOpenChange(false)} />
          <div className="mt-3">{children}</div>
          {footer && <div className="mt-4 flex justify-end gap-2">{footer}</div>}
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  )
}

/**
 * A side sheet — the default for anything that is not an interruption.
 *
 * Built on Dialog because it is the same modality with a different geometry;
 * building it on something else would give it different focus handling for no
 * reason a user could perceive.
 */
export function Sheet({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
}: SurfaceProps) {
  return (
    <BaseDialog.Root open={open} onOpenChange={onOpenChange}>
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className={backdrop} data-motion="enter" />
        <BaseDialog.Popup
          data-motion="enter"
          className={cn(
            'fixed top-0 right-0 bottom-0 z-50 flex w-[min(26rem,100vw)] flex-col',
            'border-l border-[var(--border-subtle)] bg-[var(--bg-surface)] p-4 outline-none',
          )}
        >
          <SurfaceHeader title={title} description={description} onClose={() => onOpenChange(false)} />
          <div className="mt-3 flex-1 overflow-y-auto">{children}</div>
          {footer && <div className="mt-4 flex justify-end gap-2">{footer}</div>}
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  )
}

function SurfaceHeader({
  title,
  description,
  onClose,
}: {
  title: string
  description: string | undefined
  onClose: () => void
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div>
        <BaseDialog.Title className="text-[15px] font-medium">{title}</BaseDialog.Title>
        {description && (
          <BaseDialog.Description className="mt-1 text-[13px] text-[var(--text-secondary)]">
            {description}
          </BaseDialog.Description>
        )}
      </div>
      <IconButton label="Close" onClick={onClose}>
        <Xmark width={15} height={15} />
      </IconButton>
    </div>
  )
}

export interface PopoverProps {
  trigger: ReactNode
  children: ReactNode
  /** Width in rem; popovers are small by definition, so the set is small. */
  width?: 'sm' | 'md'
  align?: 'start' | 'center' | 'end'
}

/** A popover — the least blocking surface that can still hold real content. */
export function Popover({ trigger, children, width = 'md', align = 'end' }: PopoverProps) {
  return (
    <BasePopover.Root>
      <BasePopover.Trigger render={trigger as never} />
      <BasePopover.Portal>
        <BasePopover.Positioner side="bottom" align={align} sideOffset={6}>
          <BasePopover.Popup
            data-motion="enter"
            className={cn(
              panel,
              'z-40 overflow-hidden',
              width === 'sm' ? 'w-56' : 'w-80',
            )}
          >
            {children}
          </BasePopover.Popup>
        </BasePopover.Positioner>
      </BasePopover.Portal>
    </BasePopover.Root>
  )
}
