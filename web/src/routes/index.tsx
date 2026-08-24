import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Play } from 'iconoir-react'

import { Card, Page, Section, Stack } from '@/components/layout'
import { Dropzone } from '@/components/run/Dropzone'
import { ModelPicker } from '@/components/run/ModelPicker'
import { RunOptions } from '@/components/run/Options'
import { Button, EmptyState, ErrorState, Skeleton } from '@/components/ui'
import { ApiError } from '@/lib/api/client'
import { useCatalog, useModels, useSubmitJob } from '@/lib/api/hooks'
import type { TranscribeOptions } from '@/lib/api/types'
import { remember } from '@/lib/audio-store'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { useDelayedPending } from '@/lib/pending'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'home.title',
  descriptionKey: 'home.description',
}

export const Route = createFileRoute('/')({
  component: HomePage,
  staticData: { pageMeta },
})

/** The server's own limit; the picker reports it so a refusal is not a surprise. */
const MAX_UPLOAD_BYTES = 100 * 1024 * 1024

function HomePage() {
  const t = useT()
  const navigate = useNavigate()

  const models = useModels()
  const catalog = useCatalog()
  const submit = useSubmitJob()

  const [file, setFile] = useState<File>()
  const [modelID, setModelID] = useState('')
  const [options, setOptions] = useState<TranscribeOptions>({})

  const loading = useDelayedPending(models.isLoading)
  const installed = models.data ?? []
  const chosen = installed.find((m) => m.id === modelID)

  // The first model on disk is the sensible default, and picking it here rather
  // than in an effect means the select is never briefly empty.
  const effectiveModel =
    modelID || installed.find((m) => m.kind === '' || m.kind === 'asr')?.id || ''

  async function run() {
    if (!file) return
    try {
      const job = await submit.mutateAsync({
        file,
        options: effectiveModel ? { ...options, model: effectiveModel } : options,
      })
      // The player has no other source for the audio: it is this File or
      // nothing (SPEC §2.1).
      remember(job.id, file)
      void navigate({ to: '/result/$jobId', params: { jobId: job.id } })
    } catch (err) {
      const detail = err instanceof ApiError ? err.message : String(err)
      toast.error(t('home.submitFailed'), { description: detail })
    }
  }

  if (models.isError) {
    return (
      <Page>
        <Section>
          <ErrorState
            title={t('common.error')}
            detail={String(models.error)}
            action={
              <Button size="sm" onClick={() => void models.refetch()}>
                {t('common.retry')}
              </Button>
            }
          />
        </Section>
      </Page>
    )
  }

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={4}>
            <Dropzone
              file={file}
              onFile={setFile}
              limitBytes={MAX_UPLOAD_BYTES}
              disabled={submit.isPending}
            />

            {loading && <Skeleton className="h-8 w-full" />}

            {!loading && installed.length === 0 && catalog.data?.length === 0 && (
              <EmptyState title={t('home.noModels')} description={t('home.noModelsHint')} />
            )}

            {!loading && (installed.length > 0 || (catalog.data?.length ?? 0) > 0) && (
              <ModelPicker
                models={installed}
                catalog={catalog.data ?? []}
                value={effectiveModel}
                onChange={setModelID}
                disabled={submit.isPending}
              />
            )}
          </Stack>
        </Card>
      </Section>

      <Section title={t('home.options')}>
        <Card>
          <RunOptions
            value={options}
            onChange={setOptions}
            model={chosen}
            disabled={submit.isPending}
          />
        </Card>
      </Section>

      <Section>
        <Button
          variant="primary"
          icon={<Play width={14} height={14} />}
          busy={submit.isPending}
          disabled={!file || !effectiveModel}
          onClick={() => void run()}
        >
          {t('home.run')}
        </Button>
      </Section>
    </Page>
  )
}
