import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Trash } from 'iconoir-react'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import { IsolatedResult, TranscribeIsolated, useIsolatedWav } from '@/components/isolate/IsolatedResult'
import { Voices } from '@/components/isolate/Voices'
import { Badge, Button, ErrorState, Progress, Skeleton } from '@/components/ui'
import { ApiError } from '@/lib/api/client'
import {
  useDeleteIsolation,
  useExtractVoice,
  useIsolation,
  useIsolationStream,
} from '@/lib/api/hooks'
import { isolationBusy, type Isolation, type IsolationSelection } from '@/lib/api/types'
import { forget, recall } from '@/lib/audio-store'
import { clock } from '@/lib/format'
import { currentLanguage, useT, type TKey } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'isolate.title',
  descriptionKey: 'isolate.sessionDescription',
}

export const Route = createFileRoute('/isolate/$isolationId')({
  component: IsolationPage,
  staticData: { pageMeta },
})

function IsolationPage() {
  const { isolationId } = Route.useParams()
  const t = useT()
  const navigate = useNavigate()
  const query = useIsolation(isolationId)
  const iso = query.data
  useIsolationStream(iso)
  const extract = useExtractVoice(isolationId)
  const remove = useDeleteIsolation()
  const original = recall(isolationId)

  async function choose(selection: IsolationSelection) {
    try {
      await extract.mutateAsync(selection)
    } catch (err) {
      const detail = err instanceof ApiError ? err.message : String(err)
      toast.error(t('isolate.extractFailed'), { description: detail })
    }
  }

  async function drop() {
    try {
      await remove.mutateAsync(isolationId)
      forget(isolationId)
      void navigate({ to: '/isolate' })
    } catch (err) {
      toast.error(t('common.error'), { description: err instanceof Error ? err.message : String(err) })
    }
  }

  if (query.isLoading) {
    return (
      <Page>
        <Section>
          <Skeleton className="h-[160px] w-full" />
        </Section>
      </Page>
    )
  }
  if (query.isError || !iso) {
    const gone = query.error instanceof ApiError && query.error.code === 'isolation_not_found'
    return (
      <Page>
        <Section>
          <ErrorState
            title={gone ? t('isolate.gone') : t('common.error')}
            detail={gone ? t('isolate.goneHint') : String(query.error)}
            action={
              <Button size="sm" onClick={() => void navigate({ to: '/isolate' })}>
                {t('isolate.again')}
              </Button>
            }
          />
        </Section>
      </Page>
    )
  }

  const busy = isolationBusy(iso.status)
  const expires = new Date(iso.expires_at).toLocaleTimeString(currentLanguage(), {
    hour: '2-digit',
    minute: '2-digit',
  })

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <Inline gap={3}>
              <Badge tone={statusTone(iso)}>{t(`isolate.status.${iso.status}` as TKey)}</Badge>
              {iso.filename && <span className="truncate text-[13px] text-[var(--text-muted)]">{iso.filename}</span>}
              {iso.duration !== undefined && iso.duration > 0 && (
                <span className="text-[13px] tabular-nums text-[var(--text-secondary)]">{clock(iso.duration)}</span>
              )}
            </Inline>
            {busy && <Running iso={iso} />}
            {iso.status === 'failed' && iso.error && (
              <ErrorState title={t('isolate.failed')} detail={iso.error.message} />
            )}
            {(iso.warnings ?? []).map((w) => (
              <p key={w.code} className="text-[12px] text-[var(--text-secondary)]">
                {w.message}
              </p>
            ))}
          </Stack>
        </Card>
      </Section>

      {(iso.voices?.length ?? 0) > 0 && (
        <Section title={t('isolate.voices')} description={t('isolate.voicesHint')}>
          <Voices iso={iso} busy={busy || extract.isPending} onChoose={(s) => void choose(s)} />
        </Section>
      )}

      {iso.status === 'done' && iso.result && <Done iso={iso} original={original} />}

      <Section>
        <Inline gap={3}>
          <Button variant="ghost" size="sm" icon={<Trash width={13} height={13} />} busy={remove.isPending} onClick={() => void drop()}>
            {t('isolate.delete')}
          </Button>
          <span className="text-[12px] text-[var(--text-muted)]">{t('isolate.expires', { time: expires })}</span>
        </Inline>
      </Section>
    </Page>
  )
}

function Done({ iso, original }: { iso: Isolation; original: File | undefined }) {
  const t = useT()
  const wav = useIsolatedWav(iso)
  return (
    <>
      <Section title={t('isolate.result')}>
        <Card>
          <IsolatedResult
            iso={iso}
            original={original}
            wav={wav.data}
            error={wav.error ? String(wav.error.message) : undefined}
          />
        </Card>
      </Section>
      <Section title={t('isolate.transcribe')}>
        <Card>
          <TranscribeIsolated iso={iso} wav={wav.data} />
        </Card>
      </Section>
    </>
  )
}

function Running({ iso }: { iso: Isolation }) {
  const t = useT()
  const label =
    iso.status === 'queued'
      ? iso.position
        ? t('isolate.position', { position: iso.position })
        : t('isolate.status.queued')
      : t(`isolate.stage.${iso.stage || 'decode'}` as TKey)
  return (
    <Stack gap={1}>
      <span className="text-[12px] text-[var(--text-secondary)]">{label}</span>
      <Progress percent={iso.percent ?? 0} label={label} />
    </Stack>
  )
}

function statusTone(iso: Isolation): 'neutral' | 'accent' | 'success' | 'danger' {
  switch (iso.status) {
    case 'done':
      return 'success'
    case 'failed':
      return 'danger'
    case 'awaiting_selection':
      return 'accent'
    default:
      return 'neutral'
  }
}
