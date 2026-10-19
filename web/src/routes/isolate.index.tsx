import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Search } from 'iconoir-react'

import { Card, Page, Section, Stack } from '@/components/layout'
import { Dropzone } from '@/components/run/Dropzone'
import { Button, EmptyState, ErrorState, Field, Input, Skeleton } from '@/components/ui'
import { ApiError } from '@/lib/api/client'
import { useCreateIsolation, useIsolationInfo } from '@/lib/api/hooks'
import { remember } from '@/lib/audio-store'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'isolate.title',
  descriptionKey: 'isolate.description',
}

export const Route = createFileRoute('/isolate/')({
  component: IsolatePage,
  staticData: { pageMeta },
})

const MAX_UPLOAD_BYTES = 100 * 1024 * 1024

/**
 * The first step of voice isolation: a recording in, the voices found.
 *
 * Nothing is chosen here. The server finds the voices first, and the person
 * listens to them on the next screen — a voice is picked by ear, not by a
 * speaker number nobody has heard.
 */
function IsolatePage() {
  const t = useT()
  const navigate = useNavigate()
  const info = useIsolationInfo()
  const create = useCreateIsolation()

  const [file, setFile] = useState<File>()
  const [speakers, setSpeakers] = useState('')

  async function run() {
    if (!file) return
    try {
      const n = Number.parseInt(speakers, 10)
      const iso = await create.mutateAsync({ file, ...(n > 0 ? { numSpeakers: n } : {}) })
      // The original, for listening to it beside the isolated voice; it is
      // the browser's copy, not the server's.
      remember(iso.id, file)
      void navigate({ to: '/isolate/$isolationId', params: { isolationId: iso.id } })
    } catch (err) {
      const detail = err instanceof ApiError ? err.message : String(err)
      toast.error(t('isolate.createFailed'), { description: detail })
    }
  }

  if (info.isLoading) {
    return (
      <Page>
        <Section>
          <Skeleton className="h-[140px] w-full" />
        </Section>
      </Page>
    )
  }
  if (info.isError) {
    return (
      <Page>
        <Section>
          <ErrorState title={t('common.error')} detail={String(info.error)} />
        </Section>
      </Page>
    )
  }
  if (!info.data?.enabled) {
    return (
      <Page>
        <Section>
          <EmptyState title={t('isolate.off')} description={t('isolate.offHint')} />
        </Section>
      </Page>
    )
  }

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={4}>
            <Dropzone file={file} onFile={setFile} limitBytes={MAX_UPLOAD_BYTES} disabled={create.isPending} />
            <Field label={t('isolate.numSpeakers')} description={t('isolate.numSpeakersHint')}>
              <Input
                type="number"
                min={0}
                max={8}
                inputMode="numeric"
                value={speakers}
                placeholder="0"
                onChange={(e) => setSpeakers(e.target.value)}
                disabled={create.isPending}
              />
            </Field>
          </Stack>
        </Card>
      </Section>

      <Section>
        <Stack gap={2}>
          <div>
            <Button
              variant="primary"
              icon={<Search width={14} height={14} />}
              busy={create.isPending}
              disabled={!file}
              onClick={() => void run()}
            >
              {t('isolate.find')}
            </Button>
          </div>
          <p className="text-[12px] text-[var(--text-muted)]">{t('isolate.privacy')}</p>
        </Stack>
      </Section>
    </Page>
  )
}
