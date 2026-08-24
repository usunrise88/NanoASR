import { createRootRoute, Outlet, useMatches } from '@tanstack/react-router'
import { Toaster } from 'sonner'

import { ApiKeySheet } from '@/components/ApiKeySheet'
import { AppHeader } from '@/components/AppHeader'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'

/**
 * The one and only shell.
 *
 * Everything shared lives here: header, page title block, spacing above the
 * content, and the toast viewport. Routes render content and nothing else —
 * they declare a `pageMeta` export and the shell turns it into a header, so no
 * two pages can drift apart.
 */
export const Route = createRootRoute({
  component: RootLayout,
})

function RootLayout() {
  const meta = usePageMeta()
  const t = useT()

  return (
    <div className="min-h-full">
      <AppHeader />

      <main>
        <div className="mx-auto w-full max-w-5xl px-6 pt-8 pb-4">
          {meta && (
            <div className="flex items-start justify-between gap-4">
              <div>
                <h1 className="text-[19px] font-semibold tracking-tight">{t(meta.titleKey)}</h1>
                {meta.descriptionKey && (
                  <p className="mt-1 text-[13px] text-[var(--text-secondary)]">
                    {t(meta.descriptionKey)}
                  </p>
                )}
              </div>
              {meta.actions}
            </div>
          )}
        </div>

        <Outlet />
      </main>

      {/* Stacked, auto-dismissing, quiet. Never used for critical information;
          everything raised here is also kept in the notification history. */}
      <Toaster position="bottom-right" closeButton visibleToasts={4} />

      {/* Opens itself when the server first refuses an unauthenticated
          request, which is how the SPA learns that this server wants a key. */}
      <ApiKeySheet />
    </div>
  )
}

/** Reads pageMeta from the deepest matched route. */
function usePageMeta(): PageMeta | undefined {
  const matches = useMatches()
  for (let i = matches.length - 1; i >= 0; i--) {
    const meta = (matches[i]?.staticData as { pageMeta?: PageMeta } | undefined)?.pageMeta
    if (meta) return meta
  }
  return undefined
}
