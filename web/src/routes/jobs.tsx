import { useState } from 'react'
import { createFileRoute, Link } from '@tanstack/react-router'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import {
  Badge,
  Button,
  EmptyState,
  ErrorState,
  Field,
  Select,
  SkeletonRow,
  Spinner,
} from '@/components/ui'
import { ApiError } from '@/lib/api/client'
import { useCancelJob, useJobHistory, useJobStream, useModels, shouldWatch } from '@/lib/api/hooks'
import { isTerminal, type Job, type JobStatus } from '@/lib/api/types'
import { clock, statusKey, statusTone, when } from '@/lib/format'
import { currentLanguage, useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { useDelayedPending } from '@/lib/pending'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'jobs.title',
  descriptionKey: 'jobs.description',
}

export const Route = createFileRoute('/jobs')({
  component: JobsPage,
  staticData: { pageMeta },
})

const STATUSES: JobStatus[] = ['queued', 'running', 'succeeded', 'failed', 'canceled', 'expired']

function JobsPage() {
  const t = useT()
  const models = useModels()
  const [status, setStatus] = useState('')
  const [model, setModel] = useState('')

  const filter = {
    ...(status ? { status: [status as JobStatus] } : {}),
    ...(model ? { model } : {}),
    limit: 20,
    // The history list does not need per-row transcripts: the detail screen
    // fetches them on demand. Skipping them here keeps a page of long jobs
    // a fraction of its default payload.
    includeResult: false,
  }
  const history = useJobHistory(filter)
  const loading = useDelayedPending(history.isLoading)

  const jobs = history.data?.pages.flatMap((p) => p.data) ?? []

  return (
    <Page>
      <Section>
        <Card>
          <Inline gap={4} align="start">
            <span className="w-44">
              <Field label={t('jobs.filterStatus')}>
                <Select
                  value={status}
                  onChange={setStatus}
                  options={[
                    { value: '', label: t('jobs.all') },
                    ...STATUSES.map((s) => ({ value: s, label: t(statusKey(s)) })),
                  ]}
                />
              </Field>
            </span>
            <span className="w-64">
              <Field label={t('jobs.filterModel')}>
                <Select
                  value={model}
                  onChange={setModel}
                  options={[
                    { value: '', label: t('jobs.all') },
                    ...(models.data ?? []).map((m) => ({
                      value: m.id,
                      label: m.display_name || m.id,
                    })),
                  ]}
                />
              </Field>
            </span>
          </Inline>
        </Card>
      </Section>

      <Section>
        {history.isError && (
          <ErrorState
            title={t('common.error')}
            detail={
              history.error instanceof ApiError ? history.error.message : String(history.error)
            }
            action={
              <Button size="sm" onClick={() => void history.refetch()}>
                {t('common.retry')}
              </Button>
            }
          />
        )}

        {loading && (
          <Card>
            <SkeletonRow />
            <SkeletonRow />
            <SkeletonRow />
          </Card>
        )}

        {!loading && !history.isError && jobs.length === 0 && (
          <Card>
            <EmptyState
              title={t('jobs.empty')}
              description={t('jobs.emptyHint')}
              action={
                <Link to="/">
                  <Button size="sm" variant="primary">
                    {t('nav.new')}
                  </Button>
                </Link>
              }
            />
          </Card>
        )}

        {jobs.length > 0 && (
          <Stack gap={2}>
            {jobs.map((job) => (
              <JobRow key={job.id} job={job} />
            ))}
          </Stack>
        )}

        {history.hasNextPage && (
          <Stack gap={2}>
            <Inline>
              <Button
                size="sm"
                busy={history.isFetchingNextPage}
                onClick={() => void history.fetchNextPage()}
              >
                {t('jobs.more')}
              </Button>
            </Inline>
          </Stack>
        )}
      </Section>
    </Page>
  )
}

function JobRow({ job: initial }: { job: Job }) {
  const t = useT()
  const cancel = useCancelJob()
  // Only unfinished jobs get a live connection; a finished one will never
  // change, and a stream per history row would be a stream per row forever.
  const live = useJobStream(initial.id, shouldWatch(initial))
  const job = live ?? initial
  const locale = currentLanguage()

  return (
    <Card>
      <Inline gap={3} align="start">
        <Badge tone={statusTone(job.status)}>{t(statusKey(job.status))}</Badge>

        <span className="min-w-0 flex-1">
          <Link
            to="/result/$jobId"
            params={{ jobId: job.id }}
            className="block truncate text-[13px] hover:underline"
          >
            {job.filename || job.id}
          </Link>
          <span className="block text-[12px] text-[var(--text-muted)]">
            {job.model_id} · {when(job.created_at, locale)}
            {job.result ? ` · ${clock(job.result.duration)}` : ''}
            {job.status === 'queued' && job.position
              ? ` · ${t('jobs.position', { position: job.position })}`
              : ''}
          </span>
        </span>

        {!isTerminal(job.status) && <Spinner />}

        {!isTerminal(job.status) && (
          <Button
            size="sm"
            busy={cancel.isPending}
            onClick={() => {
              void cancel
                .mutateAsync(job.id)
                .then(() => toast.info(t('jobs.canceled')))
                .catch((err: unknown) =>
                  toast.error(t('common.error'), {
                    description: err instanceof ApiError ? err.message : String(err),
                  }),
                )
            }}
          >
            {t('jobs.cancel')}
          </Button>
        )}
      </Inline>
    </Card>
  )
}
