import { useState } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import { Button, Field, Input, Select } from '@/components/ui'
import { setApiKey, useAuth } from '@/lib/auth'
import { languages, setLanguage, useT, type Language } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { setPrefs, usePrefs } from '@/lib/prefs'
import { setTheme, useTheme, type Theme } from '@/lib/theme'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'settings.title',
  descriptionKey: 'settings.description',
}

export const Route = createFileRoute('/settings')({
  component: SettingsPage,
  staticData: { pageMeta },
})

function SettingsPage() {
  const t = useT()
  const auth = useAuth()
  const theme = useTheme()
  const prefs = usePrefs()
  const qc = useQueryClient()
  const [key, setKey] = useState(auth.key)

  function saveKey() {
    setApiKey(key)
    // Whatever was refused for want of a key deserves another attempt.
    void qc.invalidateQueries()
    toast.success(t('settings.saved'))
  }

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={4}>
            <Field label={t('settings.language')}>
              <Select
                value={document.documentElement.lang || 'ru'}
                onChange={(v) => setLanguage(v as Language)}
                options={languages.map((l) => ({ value: l, label: l === 'ru' ? 'Русский' : 'English' }))}
              />
            </Field>

            <Field label={t('settings.theme')}>
              <Select
                value={theme}
                onChange={(v) => setTheme(v as Theme)}
                options={[
                  { value: 'light', label: t('theme.light') },
                  { value: 'dark', label: t('theme.dark') },
                  { value: 'system', label: t('theme.system') },
                ]}
              />
            </Field>
          </Stack>
        </Card>
      </Section>

      <Section title={t('settings.apiKey')}>
        <Card>
          <Stack gap={3}>
            <Field
              label={t('settings.apiKey')}
              description={t('settings.apiKeyHint')}
              error={auth.required && !auth.key ? t('settings.apiKeyRequired') : undefined}
            >
              <Input
                value={key}
                spellCheck={false}
                autoComplete="off"
                placeholder="sk-…"
                onChange={(e) => setKey(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') saveKey()
                }}
              />
            </Field>
            <Inline>
              <Button variant="primary" size="sm" onClick={saveKey}>
                {t('settings.save')}
              </Button>
            </Inline>
          </Stack>
        </Card>
      </Section>

      <Section title={t('settings.confidence')}>
        <Card>
          <Field label={t('settings.confidence')} description={t('settings.confidenceHint')}>
            <Input
              type="number"
              min={0}
              max={1}
              step={0.05}
              value={prefs.confidenceThreshold}
              onChange={(e) => setPrefs({ confidenceThreshold: Number(e.target.value) })}
            />
          </Field>
        </Card>
      </Section>
    </Page>
  )
}
