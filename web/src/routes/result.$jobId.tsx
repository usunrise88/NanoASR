import { useCallback, useRef, useState } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { Download } from 'iconoir-react'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import { Dropzone } from '@/components/run/Dropzone'
import { Player, type PlayerHandle } from '@/components/player/Player'
import { Transcript } from '@/components/player/Transcript'
import {
  Badge,
  Button,
  Detail,
  ErrorState,
  Input,
  Progress,
  Skeleton,
  Spinner,
} from '@/components/ui'
import { fetchTranscript } from '@/lib/api/client'
import { useJob, useJobStream, shouldWatch } from '@/lib/api/hooks'
import {
  flatWords,
  isTerminal,
  type Job,
  type ResponseFormat,
  type Result,
} from '@/lib/api/types'
import { recall, remember } from '@/lib/audio-store'
import { clock, statusKey, statusTone } from '@/lib/format'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { usePrefs } from '@/lib/prefs'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'result.title',
  descriptionKey: 'result.description',
}

export const Route = createFileRoute('/result/$jobId')({
  component: ResultPage,
  staticData: { pageMeta },
})

function ResultPage() {
  const { jobId } = Route.useParams()
  const t = useT()
  const prefs = usePrefs()

  const query = useJob(jobId)
  const live = useJobStream(jobId, shouldWatch(query.data))
  const job = live ?? query.data

  const [file, setFile] = useState<File | undefined>(() => recall(jobId))
  const [currentTime, setCurrentTime] = useState(0)
  const [search, setSearch] = useState('')
  const player = useRef<PlayerHandle>(null)

  const onReady = useCallback((handle: PlayerHandle) => {
    player.current = handle
  }, [])

  const seek = useCallback((seconds: number) => {
    player.current?.seek(seconds)
    setCurrentTime(seconds)
  }, [])

  function attach(next: File | undefined) {
    setFile(next)
    if (next) remember(jobId, next)
  }

  if (query.isLoading) {
    return (
      <Page>
        <Section>
          <Stack gap={3}>
            <Skeleton className="h-[72px] w-full" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-4/5" />
          </Stack>
        </Section>
      </Page>
    )
  }

  if (query.isError || !job) {
    return (
      <Page>
        <Section>
          <ErrorState title={t('common.error')} detail={String(query.error)} />
        </Section>
      </Page>
    )
  }

  const result = job.result
  const words = result ? flatWords(result) : []

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <Inline gap={3}>
              <Badge tone={statusTone(job.status)}>{t(statusKey(job.status))}</Badge>
              <span className="text-[13px] text-[var(--text-secondary)]">{job.model_id}</span>
              {job.filename && (
                <span className="truncate text-[13px] text-[var(--text-muted)]">{job.filename}</span>
              )}
            </Inline>

            {!isTerminal(job.status) && <RunningState job={job} />}

            {job.status === 'failed' && job.error && (
              <ErrorState title={t('result.failed')} detail={job.error.message} />
            )}

            {result && (
              <>
                {file ? (
                  <Player
                    file={file}
                    result={result}
                    words={words}
                    currentTime={currentTime}
                    onTime={setCurrentTime}
                    onReady={onReady}
                  />
                ) : (
                  <Stack gap={2}>
                    <Stack gap={1}>
                      <p className="text-[13px] font-medium">{t('result.noAudio')}</p>
                      <p className="text-[12px] text-[var(--text-secondary)]">
                        {t('result.noAudioHint')}
                      </p>
                    </Stack>
                    <Dropzone file={undefined} onFile={attach} limitBytes={100 * 1024 * 1024} />
                  </Stack>
                )}
              </>
            )}
          </Stack>
        </Card>
      </Section>

      {result && (
        <>
          <Section
            title={t('result.transcript')}
            actions={<Exports jobId={jobId} result={result} />}
          >
            <Card>
              <Stack gap={3}>
                <Input
                  value={search}
                  placeholder={t('result.search')}
                  onChange={(e) => setSearch(e.target.value)}
                />
                <Transcript
                  result={result}
                  words={words}
                  currentTime={currentTime}
                  onSeek={seek}
                  confidenceThreshold={prefs.confidenceThreshold}
                  query={search}
                />
              </Stack>
            </Card>
          </Section>

          <Section title={t('result.stats')}>
            <Card>
              <Stats result={result} t={t} />
            </Card>
          </Section>
        </>
      )}
    </Page>
  )
}

function RunningState({ job }: { job: Job }) {
  const t = useT()
  return (
    <Stack gap={2}>
      <Inline gap={2}>
        <Spinner />
        <span className="text-[13px]">
          {job.status === 'queued'
            ? t('result.waiting')
            : `${t('result.running')}${job.stage ? ` · ${job.stage}` : ''}`}
        </span>
        {job.position ? (
          <span className="text-[12px] text-[var(--text-muted)]">
            {t('jobs.position', { position: job.position })}
          </span>
        ) : null}
      </Inline>
      {job.status === 'running' && <Progress percent={job.percent ?? 0} />}
    </Stack>
  )
}

/**
 * Downloads in text formats.
 *
 * Fetched from the server rather than rendered here: Go keeps one subtitle
 * renderer for both dialects so the timecode arithmetic lives in one place, and
 * a second implementation in TypeScript would be a second place to fix it.
 */
function Exports({ jobId, result }: { jobId: string; result: Result }) {
  const t = useT()
  const [busy, setBusy] = useState<ResponseFormat>()

  async function save(format: ResponseFormat) {
    setBusy(format)
    try {
      const body =
        format === 'json' ? JSON.stringify(result, null, 2) : await fetchTranscript(jobId, format)
      const url = URL.createObjectURL(new Blob([body], { type: 'text/plain;charset=utf-8' }))
      const a = document.createElement('a')
      a.href = url
      a.download = `${jobId}.${format === 'json' ? 'json' : format}`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      toast.error(t('common.error'), { description: String(err) })
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <Inline gap={1}>
      {(['json', 'text', 'srt', 'vtt'] as ResponseFormat[]).map((format) => (
        <Button
          key={format}
          size="sm"
          busy={busy === format}
          icon={<Download width={13} height={13} />}
          onClick={() => void save(format)}
        >
          {format.toUpperCase()}
        </Button>
      ))}
    </Inline>
  )
}

function Stats({ result, t }: { result: Result; t: ReturnType<typeof useT> }) {
  const sourceKey = {
    token: 'result.timestampToken',
    segment: 'result.timestampSegment',
    aligned: 'result.timestampAligned',
  } as const

  return (
    <Stack gap={1}>
      <Detail label={t('jobs.duration')}>{clock(result.duration)}</Detail>
      <Detail label={t('result.processing')}>{clock(result.stats.processing_ms / 1000)}</Detail>
      <Detail label={t('result.rtf')}>{result.stats.rtf.toFixed(3)}</Detail>
      <Detail label={t('result.segments')}>{result.stats.segments_total}</Detail>
      <Detail label={t('result.speech')}>{Math.round(result.stats.speech_ratio * 100)}%</Detail>
      {/* Timings from model tokens and timings from segment boundaries are very
          different qualities, and a client cannot tell which it got by looking
          at the numbers. */}
      <Detail label={t('result.timestamps')}>{t(sourceKey[result.timestamp_source])}</Detail>
      {Object.entries(result.stats.stages_ms).map(([stage, ms]) => (
        <Detail key={stage} label={stage}>
          {ms} ms
        </Detail>
      ))}
      {result.warnings?.map((w) => (
        <Detail key={w.code} label={w.code}>
          {w.message}
        </Detail>
      ))}
    </Stack>
  )
}
