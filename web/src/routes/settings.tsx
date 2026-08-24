import { createFileRoute } from '@tanstack/react-router'

import { Card, Page, Section, Stack } from '@/components/layout'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'

export const pageMeta: PageMeta = {
  titleKey: 'settings.title',
  descriptionKey: 'settings.description',
}

export const Route = createFileRoute('/settings')({
  component: SettingsPage,
  staticData: { pageMeta },
})

// Filled in by the settings stage: language, theme, API key, confidence
// threshold.
function SettingsPage() {
  const t = useT()
  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <p className="text-[13px] text-[var(--text-secondary)]">{t('settings.description')}</p>
          </Stack>
        </Card>
      </Section>
    </Page>
  )
}
