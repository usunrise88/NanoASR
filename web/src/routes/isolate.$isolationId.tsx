import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { ChatLines, Group, Trash } from 'iconoir-react'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import { TranscribeSheet } from '@/components/isolate/TranscribeSheet'
import { TranscriptState } from '@/components/isolate/TranscriptState'
import { Voices } from '@/components/isolate/Voices'
import { AudioClip } from '@/components/player/AudioClip'
import { Badge, Button, ErrorState, Progress, Skeleton, Switch } from '@/components/ui'
import { ApiError, fetchIsolationAudio, fetchTrack } from '@/lib/api/client'
import {
  useDeleteIsolation,
  useExtractAll,
  useExtractVoice,
  useIsolation,
  useIsolationInfo,
  useIsolationStream,
  useTranscribeIsolation,
  useTranscribeSpeakers,
  useUpdateIsolation,
} from '@/lib/api/hooks'
import { isolationBusy, type Isolation, type IsolationSelection, type TranscribeOptions } from '@/lib/api/types'
import { forget, recall, remember } from '@/lib/audio-store'
import { voiceName } from '@/lib/conversation'
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

/** What the transcription sheet is open for: every voice, or one track. */
type Sheet = { kind: 'speakers' } | { kind: 'track'; speaker: string } | undefined

function IsolationPage() {
  const { isolationId } = Route.useParams()
  const t = useT()
  const navigate = useNavigate()
  const query = useIsolation(isolationId)
  const iso = query.data
  useIsolationStream(iso)
  const info = useIsolationInfo()
  const extract = useExtractVoice(isolationId)
  const extractAll = useExtractAll(isolationId)
  const update = useUpdateIsolation(isolationId)
  const speakers = useTranscribeSpeakers(isolationId)
  const single = useTranscribeIsolation(isolationId)
  const remove = useDeleteIsolation()
  const original = recall(isolationId)
  const [sheet, setSheet] = useState<Sheet>()

  function failed(title: string, err: unknown) {
    toast.error(title, { description: err instanceof ApiError ? err.message : String(err) })
  }

  async function choose(selection: IsolationSelection) {
    try {
      await extract.mutateAsync(selection)
    } catch (err) {
      failed(t('isolate.extractFailed'), err)
    }
  }

  async function takeAll() {
    try {
      await extractAll.mutateAsync()
    } catch (err) {
      failed(t('isolate.extractFailed'), err)
    }
  }

  async function rename(speaker: string, name: string) {
    try {
      await update.mutateAsync({ names: { [speaker]: name } })
    } catch (err) {
      failed(t('isolate.renameFailed'), err)
      throw err
    }
  }

  async function setKeep(keep: boolean) {
    try {
      await update.mutateAsync({ keep })
    } catch (err) {
      failed(t('common.error'), err)
    }
  }

  async function run(options: TranscribeOptions) {
    if (!sheet) return
    try {
      if (sheet.kind === 'speakers') {
        await speakers.mutateAsync(options)
        setSheet(undefined)
        return
      }
      const job = await single.mutateAsync({ options, speaker: sheet.speaker })
      // The result page plays what it was given: the track it was run on.
      const track = iso?.tracks?.find((tr) => tr.speaker === sheet.speaker)
      if (track) {
        try {
          const wav = await fetchTrack(isolationId, track.speaker, 'wav')
          remember(job.id, new File([wav], `${track.name}.wav`, { type: 'audio/wav' }))
        } catch {
          // Without it the result page asks for the file; the job is queued.
        }
      }
      void navigate({ to: '/result/$jobId', params: { jobId: job.id } })
    } catch (err) {
      failed(t('home.submitFailed'), err)
    }
  }

  async function drop() {
    try {
      await remove.mutateAsync(isolationId)
      forget(isolationId)
      void navigate({ to: '/isolate' })
    } catch (err) {
      failed(t('common.error'), err)
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
  const voices = iso.voices ?? []
  const canAct = voices.length > 0 && !busy
  const sampled = voices.filter((v) => v.sample).length
  const missing = voices.filter((v) => v.sample && !iso.tracks?.some((tr) => tr.speaker === v.speaker)).length

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <Inline gap={3} className="justify-between">
              <Inline gap={3}>
                <Badge tone={statusTone(iso)}>{t(`isolate.status.${iso.status}` as TKey)}</Badge>
                {iso.filename && <span className="truncate text-[13px] text-[var(--text-muted)]">{iso.filename}</span>}
                {iso.duration !== undefined && iso.duration > 0 && (
                  <span className="text-[13px] tabular-nums text-[var(--text-secondary)]">{clock(iso.duration)}</span>
                )}
              </Inline>
              {(info.data?.keep || iso.keep) && (
                <Switch
                  label={t('isolate.keepRecording')}
                  checked={iso.keep}
                  disabled={update.isPending || (busy && !iso.keep)}
                  onChange={(v) => void setKeep(v)}
                />
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
            <OriginalClip iso={iso} original={original} />
          </Stack>
        </Card>
      </Section>

      {voices.length > 0 && (
        <Section
          title={t('isolate.voices')}
          description={t('isolate.voicesHint')}
          actions={
            <Inline gap={2} className="shrink-0 flex-nowrap pl-4">
              <Button
                size="sm"
                icon={<Group width={13} height={13} />}
                disabled={!canAct || sampled === 0 || missing === 0}
                busy={extractAll.isPending}
                onClick={() => void takeAll()}
              >
                {t('isolate.extractAll')}
              </Button>
              <Button
                size="sm"
                variant="primary"
                icon={<ChatLines width={13} height={13} />}
                disabled={!canAct || (sampled === 0 && (iso.tracks?.length ?? 0) === 0)}
                onClick={() => setSheet({ kind: 'speakers' })}
              >
                {t('isolate.bySpeakers')}
              </Button>
            </Inline>
          }
        >
          <Voices
            iso={iso}
            busy={busy || extract.isPending || extractAll.isPending}
            onChoose={(s) => void choose(s)}
            onRename={rename}
            onTranscribe={(speaker) => setSheet({ kind: 'track', speaker })}
          />
        </Section>
      )}

      {iso.transcript && (
        <Section title={t('isolate.transcriptTitle')}>
          <Card>
            <TranscriptState
              iso={iso}
              onOpen={() => void navigate({ to: '/isolate/$isolationId/transcript', params: { isolationId } })}
              onRetry={() => setSheet({ kind: 'speakers' })}
            />
          </Card>
        </Section>
      )}

      <Section>
        <Inline gap={3}>
          <Button variant="ghost" size="sm" icon={<Trash width={13} height={13} />} busy={remove.isPending} onClick={() => void drop()}>
            {t('isolate.delete')}
          </Button>
          <span className="text-[12px] text-[var(--text-muted)]">
            {iso.keep || !iso.expires_at
              ? t('isolate.keptUntilDeleted')
              : t('isolate.expires', {
                  time: new Date(iso.expires_at).toLocaleTimeString(currentLanguage(), {
                    hour: '2-digit',
                    minute: '2-digit',
                  }),
                })}
          </span>
        </Inline>
      </Section>

      <TranscribeSheet
        open={sheet !== undefined}
        onOpenChange={(open) => !open && setSheet(undefined)}
        title={
          sheet?.kind === 'track'
            ? t('isolate.transcribeTrackTitle', { speaker: voiceName(voices, sheet.speaker) })
            : t('isolate.bySpeakers')
        }
        description={sheet?.kind === 'track' ? t('isolate.transcribeTrackHint') : t('isolate.bySpeakersHint')}
        submitLabel={sheet?.kind === 'track' ? t('isolate.transcribeRun') : t('isolate.bySpeakersRun')}
        busy={speakers.isPending || single.isPending}
        onRun={(options) => void run(options)}
      />
    </Page>
  )
}

/**
 * The recording itself: the browser's copy when this tab uploaded it, else
 * the server's, fetched when somebody presses play.
 */
function OriginalClip({ iso, original }: { iso: Isolation; original: File | undefined }) {
  const t = useT()
  if (!original && !(iso.duration && iso.duration > 0)) return null
  return original ? (
    <AudioClip blob={original} label={t('isolate.original')} />
  ) : (
    <AudioClip label={t('isolate.original')} load={() => fetchIsolationAudio(iso.id, 'wav')} />
  )
}

function Running({ iso }: { iso: Isolation }) {
  const t = useT()
  let label =
    iso.status === 'queued'
      ? iso.position
        ? t('isolate.position', { position: iso.position })
        : t('isolate.status.queued')
      : t(`isolate.stage.${iso.stage || 'decode'}` as TKey)
  if (iso.voice) label += ` · ${voiceName(iso.voices, iso.voice)}`
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
