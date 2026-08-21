import { createFileRoute } from '@tanstack/react-router'

import { Card, Page, Section, Stack } from '@/components/layout'
import { SkeletonRow } from '@/components/ui/Skeleton'
import type { PageMeta } from '@/lib/page'

export const pageMeta: PageMeta = {
  titleKey: 'models.title',
  descriptionKey: 'models.description',
}

export const Route = createFileRoute('/models')({
  component: ModelsPage,
  staticData: { pageMeta },
})

// TODO(M4): pool state and catalog, download progress over SSE, load / unload /
// pin / hot-swap actions, resident RSS, and the model licence — some weights
// are not licensed for commercial use and the operator has to see that.
function ModelsPage() {
  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={1}>
            <SkeletonRow />
            <SkeletonRow />
            <SkeletonRow />
          </Stack>
        </Card>
      </Section>
    </Page>
  )
}
