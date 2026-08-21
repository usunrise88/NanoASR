import type { ReactNode } from 'react'

import type { TKey } from './i18n'

/**
 * Page metadata owned by the route and rendered by the root shell.
 *
 * Titles, descriptions and page-level actions live here, never inside the
 * route's own markup. That is what keeps every page's header identical without
 * anyone having to remember to make it so (SPEC §13.2), and it is why ESLint
 * requires every route file to export one.
 */
export interface PageMeta {
  titleKey: TKey
  descriptionKey?: TKey
  actions?: ReactNode
}
