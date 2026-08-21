import { createFileRoute } from '@tanstack/react-router'

import { Card, Page, Section, Stack } from '@/components/layout'
import { SkeletonRow } from '@/components/ui/Skeleton'
import type { PageMeta } from '@/lib/page'

export const pageMeta: PageMeta = {
  titleKey: 'jobs.title',
  descriptionKey: 'jobs.description',
}

export const Route = createFileRoute('/jobs')({
  component: JobsPage,
  staticData: { pageMeta },
})

// TODO(M4): queue and history with filters and live status over SSE.
//
// History is text only: audio never outlives the active job, so a past run can
// be re-read but not re-listened to. The list says so explicitly rather than
// offering a play button that would fail.
function JobsPage() {
  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={1}>
            <SkeletonRow />
            <SkeletonRow />
          </Stack>
        </Card>
      </Section>
    </Page>
  )
}
