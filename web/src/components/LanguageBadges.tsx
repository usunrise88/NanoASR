import { Badge } from '@/components/ui'
import type { ModelInfo } from '@/lib/api/types'
import { useT } from '@/lib/i18n'

/**
 * What a model is for. "multi" alone says nothing a person can use; the number
 * of locales it can be told, and whether it also streams, do.
 */
export function LanguageBadges({ model }: { model: ModelInfo }) {
  const t = useT()
  const count = model.locales?.length ?? 0
  return (
    <>
      {count > 0 ? (
        <Badge>{t('models.multilingual', { count })}</Badge>
      ) : (
        model.languages.map((l) => <Badge key={l}>{l}</Badge>)
      )}
      {model.streaming && <Badge>{t('models.streaming')}</Badge>}
    </>
  )
}
