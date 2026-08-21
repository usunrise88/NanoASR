import { createFileRoute } from '@tanstack/react-router'

import { Card, Page, Section, Stack } from '@/components/layout'
import { Skeleton } from '@/components/ui/Skeleton'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'

export const pageMeta: PageMeta = {
  titleKey: 'home.title',
  descriptionKey: 'home.description',
}

export const Route = createFileRoute('/')({
  component: HomePage,
  staticData: { pageMeta },
})

// TODO(M4): dropzone, model picker with catalog download, request options,
// submit; on success navigate to /result/$jobId with the local File in router
// state so the player can use an object URL (SPEC §2.1).
function HomePage() {
  const t = useT()
  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <p className="text-[13px] text-[var(--text-secondary)]">{t('home.drop')}</p>
            <Skeleton className="h-32 w-full" />
          </Stack>
        </Card>
      </Section>
    </Page>
  )
}
