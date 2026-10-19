import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { Download, Play } from 'iconoir-react'

import { Inline, Stack } from '@/components/layout'
import { AudioClip } from '@/components/player/AudioClip'
import { ModelPicker } from '@/components/run/ModelPicker'
import { RunOptions } from '@/components/run/Options'
import { Button } from '@/components/ui'
import { ApiError, fetchIsolated } from '@/lib/api/client'
import { useCatalog, useModels, useTranscribeIsolation } from '@/lib/api/hooks'
import type { AudioFormat, Isolation, TranscribeOptions } from '@/lib/api/types'
import { remember } from '@/lib/audio-store'
import { useT } from '@/lib/i18n'
import { toast } from '@/lib/toast'

/**
 * The isolated voice as WAV, fetched once per extraction: it is what the
 * player plays, what "download WAV" saves and what the result page plays
 * after transcription.
 */
export function useIsolatedWav(iso: Isolation) {
  return useQuery({
    queryKey: ['isolated-wav', resultKey(iso)],
    queryFn: () => fetchIsolated(iso.id, 'wav'),
    enabled: iso.status === 'done' && iso.result !== undefined,
    staleTime: Infinity,
    gcTime: 60_000,
    retry: false,
  })
}

/** The isolated voice beside the original, and its downloads. */
export function IsolatedResult({
  iso,
  original,
  wav,
  error,
}: {
  iso: Isolation
  original: File | undefined
  wav: Blob | undefined
  error: string | undefined
}) {
  const t = useT()
  const result = iso.result
  const [downloading, setDownloading] = useState<AudioFormat>()

  async function download(format: AudioFormat) {
    if (!result) return
    setDownloading(format)
    try {
      const blob = format === 'wav' && wav ? wav : await fetchIsolated(iso.id, format)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `${result.name}.${format}`
      a.click()
      setTimeout(() => URL.revokeObjectURL(url), 10_000)
    } catch (err) {
      toast.error(t('isolate.downloadFailed'), { description: err instanceof Error ? err.message : String(err) })
    } finally {
      setDownloading(undefined)
    }
  }

  if (!result) return null
  return (
    <Stack gap={4}>
      <Stack gap={3}>
        {original && <AudioClip blob={original} label={t('isolate.original')} />}
        <AudioClip blob={wav} label={t('isolate.isolated', { speaker: result.speaker })} />
        {error && <p className="text-[12px] text-[var(--danger-text)]">{error}</p>}
      </Stack>
      <Inline gap={2}>
        {(iso.formats ?? ['wav']).map((format) => (
          <Button
            key={format}
            size="sm"
            icon={<Download width={13} height={13} />}
            busy={downloading === format}
            disabled={downloading !== undefined}
            onClick={() => void download(format)}
          >
            {t('isolate.download', { format: format.toUpperCase() })}
          </Button>
        ))}
      </Inline>
    </Stack>
  )
}

/** Sends the isolated voice to the queue with the run screen's options. */
export function TranscribeIsolated({ iso, wav }: { iso: Isolation; wav: Blob | undefined }) {
  const t = useT()
  const navigate = useNavigate()
  const models = useModels()
  const catalog = useCatalog()
  const submit = useTranscribeIsolation(iso.id)
  const [modelID, setModelID] = useState('')
  const [options, setOptions] = useState<TranscribeOptions>({})

  const installed = models.data ?? []
  const effectiveModel = modelID || installed.find((m) => m.kind === '' || m.kind === 'asr')?.id || ''
  const chosen = installed.find((m) => m.id === effectiveModel)

  async function run() {
    try {
      const job = await submit.mutateAsync(effectiveModel ? { ...options, model: effectiveModel } : options)
      // The result page plays what it was given; this is the voice it was run on.
      if (wav && iso.result) remember(job.id, new File([wav], `${iso.result.name}.wav`, { type: 'audio/wav' }))
      void navigate({ to: '/result/$jobId', params: { jobId: job.id } })
    } catch (err) {
      const detail = err instanceof ApiError ? err.message : String(err)
      toast.error(t('home.submitFailed'), { description: detail })
    }
  }

  return (
    <Stack gap={3}>
      {(installed.length > 0 || (catalog.data?.length ?? 0) > 0) && (
        <ModelPicker
          models={installed}
          catalog={catalog.data ?? []}
          value={effectiveModel}
          onChange={setModelID}
          disabled={submit.isPending}
        />
      )}
      <RunOptions value={options} onChange={setOptions} model={chosen} disabled={submit.isPending} />
      <div>
        <Button
          variant="primary"
          icon={<Play width={14} height={14} />}
          busy={submit.isPending}
          disabled={!effectiveModel}
          onClick={() => void run()}
        >
          {t('isolate.transcribeRun')}
        </Button>
      </div>
    </Stack>
  )
}

/** Identifies one extraction, for keying IsolatedResult. */
export function resultKey(iso: Isolation): string {
  return `${iso.id}:${iso.result?.speaker ?? ''}:${iso.selected?.sample_start ?? ''}`
}
