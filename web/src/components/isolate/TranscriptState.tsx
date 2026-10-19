import { ChatLines } from 'iconoir-react'

import { Inline, Stack } from '@/components/layout'
import { Button, ErrorState, Progress } from '@/components/ui'
import { isolationBusy, type Isolation } from '@/lib/api/types'
import { voiceName } from '@/lib/conversation'
import { useT } from '@/lib/i18n'
import { speakerIndex } from '@/lib/speakers'

import { VoiceBadge } from './VoiceName'

/** Where the transcript by speaker is, and the way to it. */
export function TranscriptState({ iso, onOpen, onRetry }: { iso: Isolation; onOpen: () => void; onRetry: () => void }) {
  const t = useT()
  const tr = iso.transcript
  if (!tr) return null
  if (tr.status === 'failed') {
    return (
      <ErrorState
        title={t('isolate.transcriptFailed')}
        detail={tr.error?.message ?? ''}
        action={
          <Button size="sm" disabled={isolationBusy(iso.status)} onClick={onRetry}>
            {t('isolate.bySpeakersAgain')}
          </Button>
        }
      />
    )
  }
  if (tr.status !== 'succeeded') {
    return (
      <Stack gap={2}>
        <span className="text-[13px]">
          {tr.status === 'queued' ? t('isolate.transcriptWaiting') : t('isolate.transcriptRunning')}
        </span>
        {(tr.jobs ?? []).map((job) => (
          <div key={job.job_id} className="flex items-center gap-2">
            <VoiceBadge name={voiceName(iso.voices, job.speaker)} index={speakerIndex(iso.voices, job.speaker)} size="sm" />
            <span className="w-32 truncate text-[12px]">{voiceName(iso.voices, job.speaker)}</span>
            <div className="flex-1">
              <Progress percent={job.percent ?? 0} />
            </div>
          </div>
        ))}
      </Stack>
    )
  }
  return (
    <Stack gap={2}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="text-[13px] text-[var(--text-secondary)]">
          {t('isolate.transcriptReady', { model: tr.model ?? '', voices: tr.jobs?.length ?? 0 })}
        </span>
        <Inline gap={2}>
          {tr.stale && (
            <Button size="sm" variant="ghost" disabled={isolationBusy(iso.status)} onClick={onRetry}>
              {t('isolate.bySpeakersAgain')}
            </Button>
          )}
          <Button size="sm" variant="primary" icon={<ChatLines width={13} height={13} />} onClick={onOpen}>
            {t('isolate.openTranscript')}
          </Button>
        </Inline>
      </div>
      {tr.stale && <p className="text-[12px] text-[var(--text-secondary)]">{t('isolate.transcriptStale')}</p>}
    </Stack>
  )
}

