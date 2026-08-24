import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { Button, Field, Input, Sheet } from '@/components/ui'
import { Stack } from '@/components/layout'
import { setApiKey, useAuth } from '@/lib/auth'
import { useT } from '@/lib/i18n'

/**
 * Asks for the API key, when and only when the server has said it needs one.
 *
 * There is no configuration flag telling the SPA whether authentication is on.
 * The first 401 says so, which is both fewer moving parts and impossible to
 * get out of step with what the server actually enforces — a flag could say
 * "open" while the server refused every request.
 */
export function ApiKeySheet() {
  const t = useT()
  const auth = useAuth()
  const qc = useQueryClient()
  const [dismissed, setDismissed] = useState(false)
  const [draft, setDraft] = useState(auth.key)

  // Whether the sheet is open is not state of its own: it is what the server
  // has said, minus what the user has dismissed. Deriving it means there is no
  // effect that could open it a second time or leave it open after a key is
  // set. A user who dismisses it is choosing to go to Settings instead.
  const open = auth.required && !auth.key && !dismissed

  function save() {
    setApiKey(draft)
    // Everything that failed with a 401 is worth another try now.
    void qc.invalidateQueries()
  }

  return (
    <Sheet
      open={open}
      onOpenChange={(next) => setDismissed(!next)}
      title={t('settings.apiKeyRequired')}
      description={t('settings.apiKeyRequiredHint')}
      footer={
        <>
          <Button onClick={() => setDismissed(true)}>{t('common.close')}</Button>
          <Button variant="primary" onClick={save} disabled={draft.trim() === ''}>
            {t('settings.save')}
          </Button>
        </>
      }
    >
      <Stack gap={3}>
        <Field label={t('settings.apiKey')} description={t('settings.apiKeyHint')}>
          <Input
            value={draft}
            autoFocus
            spellCheck={false}
            autoComplete="off"
            placeholder="sk-…"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && draft.trim() !== '') save()
            }}
          />
        </Field>
      </Stack>
    </Sheet>
  )
}
