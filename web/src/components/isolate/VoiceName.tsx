import { useState } from 'react'
import { EditPencil } from 'iconoir-react'

import { IconButton, Input } from '@/components/ui'
import { cn } from '@/lib/cn'
import { initials } from '@/lib/conversation'
import { useT } from '@/lib/i18n'
import { speakerTone } from '@/lib/speakers'

/** A voice's round badge in its colour: initials, or its number. */
export function VoiceBadge({ name, index, size = 'md' }: { name: string; index: number; size?: 'sm' | 'md' }) {
  const tone = speakerTone(index)
  return (
    <span
      aria-hidden
      className={cn(
        'inline-flex shrink-0 items-center justify-center rounded-full font-medium text-white',
        tone.solid,
        size === 'md' ? 'h-7 w-7 text-[11px]' : 'h-5 w-5 text-[10px]',
      )}
    >
      {initials(name, index)}
    </span>
  )
}

/**
 * A voice's name, edited in place: the pencil opens a field, Enter or leaving
 * it saves, Escape puts it back. An empty name gives the voice its id back.
 */
export function VoiceName({
  speaker,
  name,
  index,
  onRename,
  disabled,
}: {
  speaker: string
  name: string | undefined
  index: number
  // Without onRename the name is shown and cannot be changed: a diarized
  // job's speakers have nowhere to keep one.
  onRename?: ((name: string) => Promise<void>) | undefined
  disabled?: boolean
}) {
  const t = useT()
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(name ?? '')
  const shown = name || speaker
  const tone = speakerTone(index)

  async function save() {
    setEditing(false)
    if (draft.trim() === (name ?? '')) return
    try {
      await onRename?.(draft.trim())
    } catch {
      setDraft(name ?? '')
    }
  }

  if (editing) {
    return (
      <span className="inline-flex items-center gap-2">
        <VoiceBadge name={draft || speaker} index={index} />
        <Input
          autoFocus
          className="h-7 w-44"
          value={draft}
          maxLength={64}
          placeholder={speaker}
          aria-label={t('isolate.nameLabel', { speaker })}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={() => void save()}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void save()
            if (e.key === 'Escape') {
              setDraft(name ?? '')
              setEditing(false)
            }
          }}
        />
      </span>
    )
  }
  return (
    <span className="inline-flex min-w-0 items-center gap-2">
      <VoiceBadge name={shown} index={index} />
      <span className={cn('truncate text-[13px] font-medium', tone.text)} data-voice-name>
        {shown}
      </span>
      {name && <span className="text-[11px] text-[var(--text-muted)]">{speaker}</span>}
      {onRename && (
        <IconButton
          label={t('isolate.rename')}
          disabled={disabled}
          onClick={() => {
            setDraft(name ?? '')
            setEditing(true)
          }}
        >
          <EditPencil width={13} height={13} />
        </IconButton>
      )}
    </span>
  )
}
