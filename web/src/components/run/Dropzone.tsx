import { useRef, useState, type DragEvent } from 'react'
import { MediaVideo, Xmark } from 'iconoir-react'

import { Button, IconButton } from '@/components/ui'
import { cn } from '@/lib/cn'
import { bytes } from '@/lib/format'
import { useT } from '@/lib/i18n'

/**
 * Picking a file — by drop, by click, or by keyboard.
 *
 * The file never leaves the browser except as the body of one request: nothing
 * here uploads on selection, so choosing the wrong file costs nothing.
 */
export function Dropzone({
  file,
  onFile,
  limitBytes,
  disabled,
}: {
  file: File | undefined
  onFile: (file: File | undefined) => void
  limitBytes: number
  disabled?: boolean | undefined
}) {
  const t = useT()
  const input = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)

  function accept(list: FileList | null) {
    const next = list?.[0]
    if (next) onFile(next)
  }

  function onDrop(e: DragEvent) {
    e.preventDefault()
    setOver(false)
    if (!disabled) accept(e.dataTransfer.files)
  }

  if (file) {
    return (
      <div className="flex items-center gap-3 rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-canvas)] p-3">
        <MediaVideo width={18} height={18} className="shrink-0 text-[var(--text-muted)]" />
        <div className="min-w-0 flex-1">
          <p className="truncate text-[13px]">{file.name}</p>
          <p className="text-[12px] text-[var(--text-muted)]">{bytes(file.size)}</p>
        </div>
        <Button size="sm" onClick={() => input.current?.click()} disabled={disabled}>
          {t('home.replace')}
        </Button>
        <IconButton label={t('common.cancel')} onClick={() => onFile(undefined)} disabled={disabled}>
          <Xmark width={15} height={15} />
        </IconButton>
        <input
          ref={input}
          type="file"
          accept="audio/*,video/*"
          className="hidden"
          onChange={(e) => accept(e.target.files)}
        />
      </div>
    )
  }

  return (
    <div
      role="button"
      tabIndex={disabled ? -1 : 0}
      aria-label={t('home.drop')}
      onClick={() => !disabled && input.current?.click()}
      onKeyDown={(e) => {
        if (disabled) return
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          input.current?.click()
        }
      }}
      onDragOver={(e) => {
        e.preventDefault()
        setOver(true)
      }}
      onDragLeave={() => setOver(false)}
      onDrop={onDrop}
      className={cn(
        'flex cursor-pointer flex-col items-center gap-1 rounded-[var(--radius-md)]',
        'border border-dashed border-[var(--border-default)] bg-[var(--bg-canvas)] px-6 py-10 text-center',
        'hover:border-[var(--accent-solid)] focus-visible:outline focus-visible:outline-2',
        'focus-visible:outline-[var(--accent-solid)]',
        over && 'border-[var(--accent-solid)] bg-[var(--accent-bg)]',
        disabled && 'pointer-events-none opacity-50',
      )}
    >
      <p className="text-[13px]">{t('home.drop')}</p>
      <p className="text-[12px] text-[var(--text-muted)]">
        {t('home.dropHint', { limit: bytes(limitBytes) })}
      </p>
      <input
        ref={input}
        type="file"
        accept="audio/*,video/*"
        className="hidden"
        onChange={(e) => accept(e.target.files)}
      />
    </div>
  )
}
