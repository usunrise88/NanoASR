import { useState } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { Download, Pin, PinSolid, Refresh } from 'iconoir-react'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import {
  Badge,
  Button,
  Detail,
  Dialog,
  EmptyState,
  ErrorState,
  Progress,
  Skeleton,
  SkeletonRow,
} from '@/components/ui'
import { ApiError } from '@/lib/api/client'
import {
  useCatalog,
  useLoadModel,
  useModelDownload,
  useModels,
  usePinModel,
  useReloadModel,
  useUnloadModel,
} from '@/lib/api/hooks'
import type { ModelInfo } from '@/lib/api/types'
import { bytes, modelStateKey, modelTone } from '@/lib/format'
import { useAuth } from '@/lib/auth'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { useDelayedPending } from '@/lib/pending'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'models.title',
  descriptionKey: 'models.description',
}

export const Route = createFileRoute('/models')({
  component: ModelsPage,
  staticData: { pageMeta },
})

function ModelsPage() {
  const t = useT()
  const auth = useAuth()
  const models = useModels()
  const catalog = useCatalog()
  const loading = useDelayedPending(models.isLoading)

  const installed = models.data ?? []
  const onDisk = new Set(installed.map((m) => m.id))
  const missing = (catalog.data ?? []).filter((m) => !onDisk.has(m.id))

  // Actions that change server state need an admin key. Hiding them beats
  // showing buttons that answer 403 — the key cannot be fixed by clicking.
  const canAdminister = !auth.required || auth.key !== ''

  if (models.isError) {
    return (
      <Page>
        <Section>
          <ErrorState
            title={t('common.error')}
            detail={models.error instanceof ApiError ? models.error.message : String(models.error)}
            action={
              <Button size="sm" onClick={() => void models.refetch()}>
                {t('common.retry')}
              </Button>
            }
          />
        </Section>
      </Page>
    )
  }

  return (
    <Page>
      <Section title={t('models.installed')}>
        {loading && (
          <Card>
            <SkeletonRow />
            <SkeletonRow />
          </Card>
        )}
        {!loading && installed.length === 0 && (
          <Card>
            <EmptyState title={t('home.noModels')} description={t('home.noModelsHint')} />
          </Card>
        )}
        {!loading && installed.length > 0 && (
          <Stack gap={2}>
            {installed.map((model) => (
              <InstalledModel key={model.id} model={model} canAdminister={canAdminister} />
            ))}
          </Stack>
        )}
      </Section>

      {missing.length > 0 && (
        <Section title={t('models.catalog')}>
          <Stack gap={2}>
            {missing.map((model) => (
              <CatalogModel key={model.id} model={model} canAdminister={canAdminister} />
            ))}
          </Stack>
        </Section>
      )}

      {catalog.isLoading && <Skeleton className="h-16 w-full" />}
    </Page>
  )
}

function InstalledModel({
  model,
  canAdminister,
}: {
  model: ModelInfo
  canAdminister: boolean
}) {
  const t = useT()
  const load = useLoadModel()
  const unload = useUnloadModel()
  const pin = usePinModel()
  const reload = useReloadModel()
  const [confirmUnload, setConfirmUnload] = useState(false)

  const busy = load.isPending || unload.isPending || pin.isPending || reload.isPending

  // Every model action fails the same way — a 403 for the wrong key, a 404 for
  // a model that went away — so they report it the same way.
  function report(err: unknown) {
    toast.error(t('models.actionFailed'), {
      description: err instanceof ApiError ? err.message : String(err),
    })
  }

  return (
    <Card>
      <Stack gap={3}>
        <Inline gap={2}>
          <span className="text-[13px] font-medium">{model.display_name || model.id}</span>
          <Badge tone={modelTone(model.state)}>{t(modelStateKey(model.state))}</Badge>
          {model.pinned && <Badge tone="accent">{t('models.pin')}</Badge>}
          {model.languages.map((l) => (
            <Badge key={l}>{l}</Badge>
          ))}
        </Inline>

        <Stack gap={1}>
          <Detail label={t('models.revision')}>{model.revision}</Detail>
          {/* A model that is not resident reports the manifest's estimate, not
              anything it is using; saying "Memory" for that would claim a
              gigabyte of RAM is gone when nothing is loaded. */}
          {model.rss_mb > 0 && (
            <Detail label={model.state === 'ready' ? t('models.memory') : t('models.memoryEstimate')}>
              {bytes(model.rss_mb * 1024 * 1024)}
            </Detail>
          )}
          {model.ref_count > 0 && <Detail label={t('models.refs')}>{model.ref_count}</Detail>}
          {model.license && <Detail label={t('models.license')}>{model.license}</Detail>}
        </Stack>

        {canAdminister ? (
          <Inline gap={2}>
            {model.state !== 'ready' && (
              <Button size="sm" busy={load.isPending} onClick={() => void load.mutateAsync([model.id]).catch(report)}>
                {t('models.load')}
              </Button>
            )}
            {model.state === 'ready' && (
              <Button size="sm" busy={unload.isPending} onClick={() => setConfirmUnload(true)}>
                {t('models.unload')}
              </Button>
            )}
            <Button
              size="sm"
              busy={pin.isPending}
              icon={model.pinned ? <PinSolid width={13} height={13} /> : <Pin width={13} height={13} />}
              onClick={() => void pin.mutateAsync([model.id, !model.pinned]).catch(report)}
            >
              {model.pinned ? t('models.unpin') : t('models.pin')}
            </Button>
            <Button
              size="sm"
              busy={reload.isPending}
              icon={<Refresh width={13} height={13} />}
              onClick={() => void reload.mutateAsync([model.id, model.revision]).catch(report)}
            >
              {t('models.reload')}
            </Button>
            {busy && <span className="text-[12px] text-[var(--text-muted)]">…</span>}
          </Inline>
        ) : (
          <p className="text-[12px] text-[var(--text-muted)]">{t('models.adminOnly')}</p>
        )}
      </Stack>

      <Dialog
        open={confirmUnload}
        onOpenChange={setConfirmUnload}
        title={t('models.confirmUnload')}
        description={t('models.confirmUnloadBody')}
        footer={
          <>
            <Button onClick={() => setConfirmUnload(false)}>{t('common.cancel')}</Button>
            <Button
              variant="danger"
              onClick={() => {
                setConfirmUnload(false)
                void unload.mutateAsync([model.id]).catch(report)
              }}
            >
              {t('models.unload')}
            </Button>
          </>
        }
      >
        <p className="text-[13px]">{model.display_name || model.id}</p>
      </Dialog>
    </Card>
  )
}

function CatalogModel({ model, canAdminister }: { model: ModelInfo; canAdminister: boolean }) {
  const t = useT()
  const download = useModelDownload()

  return (
    <Card>
      <Stack gap={3}>
        <Inline gap={2}>
          <span className="text-[13px] font-medium">{model.display_name || model.id}</span>
          <Badge>{t('status.absent')}</Badge>
          {model.languages.map((l) => (
            <Badge key={l}>{l}</Badge>
          ))}
          {model.license && (
            <span className="text-[11px] text-[var(--text-muted)]">{model.license}</span>
          )}
        </Inline>

        {download.running && download.progress ? (
          <Progress percent={download.progress.percent} label={t('models.download')} />
        ) : (
          canAdminister && (
            <Inline>
              <Button
                size="sm"
                icon={<Download width={13} height={13} />}
                onClick={() => download.start(model.id)}
              >
                {t('models.download')}
              </Button>
            </Inline>
          )
        )}
        {download.error && (
          <p className="text-[12px] text-[var(--danger-text)]">{download.error}</p>
        )}
      </Stack>
    </Card>
  )
}
