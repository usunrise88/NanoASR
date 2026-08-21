import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createRouter, RouterProvider } from '@tanstack/react-router'

import type { PageMeta } from '@/lib/page'
import { applyTheme, watchSystemTheme } from '@/lib/theme'
import '@/lib/i18n'
import '@/styles/index.css'

import { routeTree } from './routeTree.gen'

// defaultPreload: 'intent' fetches a route's data on hover, so navigation
// usually has nothing left to wait for and no spinner to show (SPEC §13.4).
const router = createRouter({
  routeTree,
  basepath: '/ui',
  defaultPreload: 'intent',
  defaultPreloadStaleTime: 10_000,
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
  interface StaticDataRouteOption {
    pageMeta?: PageMeta
  }
}

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 5_000, retry: 1 } },
})

applyTheme()
watchSystemTheme()

const rootEl = document.getElementById('root')
if (!rootEl) throw new Error('#root is missing from index.html')

createRoot(rootEl).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
