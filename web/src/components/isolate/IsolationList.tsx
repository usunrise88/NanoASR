import { Link } from '@tanstack/react-router'
import { BookmarkSolid } from 'iconoir-react'

import { Badge } from '@/components/ui'
import type { Isolation } from '@/lib/api/types'
import { clock } from '@/lib/format'
import { currentLanguage, useT, type TKey } from '@/lib/i18n'
import { speakerTone } from '@/lib/speakers'

/**
 * The isolations this key can open, the newest first: the kept ones a person
 * comes back to, and the ones still within their time.
 */
export function IsolationList({ isolations }: { isolations: Isolation[] }) {
  const t = useT()
  const lang = currentLanguage()
  return (
    <div className="flex flex-col divide-y divide-[var(--border-subtle)]" data-isolations>
      {isolations.map((iso) => {
        const voices = iso.voices ?? []
        const transcribed = iso.transcript?.status === 'succeeded'
        return (
          <Link
            key={iso.id}
            to={transcribed ? '/isolate/$isolationId/transcript' : '/isolate/$isolationId'}
            params={{ isolationId: iso.id }}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 px-1 py-2.5 hover:bg-[var(--bg-hover)]"
            data-isolation={iso.id}
          >
            {iso.keep ? (
              <BookmarkSolid width={14} height={14} className="text-[var(--accent-text)]" aria-label={t('isolate.kept')} />
            ) : (
              <span className="w-[14px]" />
            )}
            <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{iso.filename || iso.id}</span>
            <span className="flex items-center gap-1">
              {voices.slice(0, 6).map((v, i) => (
                <span
                  key={v.speaker}
                  title={v.name || v.speaker}
                  className={`h-2.5 w-2.5 rounded-full ${speakerTone(i).solid}`}
                />
              ))}
            </span>
            {iso.duration ? (
              <span className="text-[12px] tabular-nums text-[var(--text-secondary)]">{clock(iso.duration)}</span>
            ) : null}
            {transcribed ? (
              <Badge tone="success">{t('isolate.hasTranscript')}</Badge>
            ) : (
              <Badge tone={iso.status === 'failed' ? 'danger' : 'neutral'}>
                {t(`isolate.status.${iso.status}` as TKey)}
              </Badge>
            )}
            <span className="w-28 text-right text-[12px] tabular-nums text-[var(--text-muted)]">
              {new Date(iso.created_at).toLocaleString(lang, {
                day: '2-digit',
                month: 'short',
                hour: '2-digit',
                minute: '2-digit',
              })}
            </span>
          </Link>
        )
      })}
    </div>
  )
}
