import { createFileRoute } from '@tanstack/react-router'

import { Card, Page, Section, Stack } from '@/components/layout'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'

export const pageMeta: PageMeta = {
  titleKey: 'result.title',
  descriptionKey: 'result.description',
}

export const Route = createFileRoute('/result/$jobId')({
  component: ResultPage,
  staticData: { pageMeta },
})

// Filled in by the player stage.
function ResultPage() {
  const t = useT()
  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <p className="text-[13px] text-[var(--text-secondary)]">{t('result.description')}</p>
          </Stack>
        </Card>
      </Section>
    </Page>
  )
}
