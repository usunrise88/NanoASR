import { useRef, useState } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { Download, Plus, Trash, Upload } from 'iconoir-react'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import {
  Badge,
  Button,
  Detail,
  Dialog,
  EmptyState,
  ErrorState,
  Field,
  Input,
  Skeleton,
  SkeletonRow,
  Textarea,
} from '@/components/ui'
import { ApiError, exportDictionary } from '@/lib/api/client'
import {
  useCreateDictionary,
  useDeleteDictionary,
  useDictionaries,
  useDictionary,
  useImportDictionary,
  useModels,
  useReplaceDictionary,
} from '@/lib/api/hooks'
import type { Dictionary, HotwordPolicy, ModelInfo } from '@/lib/api/types'
import { useAuth } from '@/lib/auth'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'hotwords.title',
  descriptionKey: 'hotwords.description',
}

export const Route = createFileRoute('/hotwords')({
  component: HotwordsPage,
  staticData: { pageMeta },
})

function HotwordsPage() {
  const t = useT()
  const auth = useAuth()
  const [query, setQuery] = useState('')
  const [editing, setEditing] = useState<string | undefined>()
  const [creating, setCreating] = useState(false)

  const list = useDictionaries(query)
  const models = useModels()

  // Writing needs an administrative key, as model administration does: a
  // dictionary changes what every caller's transcripts come out as.
  const canAdminister = !auth.required || auth.key !== ''

  if (list.isError) {
    const err = list.error
    const unavailable = err instanceof ApiError && err.status === 501
    return (
      <Page>
        <Section>
          <ErrorState
            title={unavailable ? t('hotwords.unavailable') : t('common.error')}
            detail={err instanceof ApiError ? err.message : String(err)}
            {...(unavailable
              ? {}
              : {
                  action: (
                    <Button size="sm" onClick={() => void list.refetch()}>
                      {t('common.retry')}
                    </Button>
                  ),
                })}
          />
        </Section>
      </Page>
    )
  }

  const dictionaries = list.data?.data ?? []
  const policy = list.data?.policy

  return (
    <Page>
      <Section>
        <Stack gap={3}>
          {policy && <PolicyNote policy={policy} />}
          <SupportedModels models={models.data ?? []} />
        </Stack>
      </Section>

      <Section
        title={t('hotwords.list')}
        actions={
          canAdminister && (
            <Button
              size="sm"
              icon={<Plus width={13} height={13} />}
              onClick={() => setCreating(true)}
            >
              {t('hotwords.new')}
            </Button>
          )
        }
      >
        <Stack gap={3}>
          <Field label={t('hotwords.search')}>
            <Input
              value={query}
              spellCheck={false}
              placeholder={t('hotwords.searchHint')}
              onChange={(e) => setQuery(e.target.value)}
            />
          </Field>

          {list.isLoading && (
            <Card>
              <SkeletonRow />
              <SkeletonRow />
            </Card>
          )}

          {!list.isLoading && dictionaries.length === 0 && (
            <Card>
              <EmptyState
                title={query ? t('hotwords.noMatches') : t('hotwords.empty')}
                description={query ? t('hotwords.noMatchesHint') : t('hotwords.emptyHint')}
              />
            </Card>
          )}

          {dictionaries.map((d) => (
            <DictionaryCard
              key={d.key}
              dictionary={d}
              canAdminister={canAdminister}
              onEdit={() => setEditing(d.key)}
            />
          ))}
        </Stack>
      </Section>

      {creating && <DictionaryEditor onClose={() => setCreating(false)} />}
      {editing !== undefined && (
        <DictionaryEditor editKey={editing} onClose={() => setEditing(undefined)} />
      )}
    </Page>
  )
}

/**
 * What the server will do with these lists.
 *
 * Shown before the lists themselves, because a dictionary curated on a server
 * where biasing is switched off is an hour of somebody's work that changes
 * nothing, and the setting that would change that is not visible from here.
 */
function PolicyNote({ policy }: { policy: HotwordPolicy }) {
  const t = useT()
  if (policy.enabled && policy.max_variants > 0) {
    return (
      <p className="text-[12px] text-[var(--text-muted)]">
        {t('hotwords.policyOn', { score: policy.default_score })}
      </p>
    )
  }
  return (
    <Card>
      <Stack gap={1}>
        <span className="text-[13px] font-medium">{t('hotwords.policyOff')}</span>
        <p className="text-[12px] text-[var(--text-secondary)]">
          {policy.enabled ? t('hotwords.policyNoVariants') : t('hotwords.policyDisabled')}
        </p>
      </Stack>
    </Card>
  )
}

/**
 * Which models a dictionary can actually be applied to.
 *
 * The question has a different answer for every model and no answer at all in
 * the configuration: biasing happens inside the decoder, so a model either
 * carries the machinery or does not. The server computes it; this only prints
 * it, together with the reason, which is usually a missing vocabulary file
 * rather than anything an operator can switch on.
 */
function SupportedModels({ models }: { models: ModelInfo[] }) {
  const t = useT()
  const asr = models.filter((m) => m.kind === 'asr')
  if (asr.length === 0) return null

  const supported = asr.filter((m) => m.capabilities.hotwords)
  const unsupported = asr.filter((m) => !m.capabilities.hotwords)

  return (
    <Card>
      <Stack gap={2}>
        <span className="text-[13px] font-medium">{t('hotwords.models')}</span>
        {supported.length === 0 ? (
          <p className="text-[12px] text-[var(--text-secondary)]">{t('hotwords.modelsNone')}</p>
        ) : (
          <Inline gap={2}>
            {supported.map((m) => (
              <Badge key={m.id} tone="accent">
                {m.display_name || m.id}
              </Badge>
            ))}
          </Inline>
        )}
        <Stack gap={1}>
          {unsupported.map((m) => (
            <Detail key={m.id} label={m.display_name || m.id}>
              {m.capabilities.hotwords_reason ?? t('hotwords.modelNo')}
            </Detail>
          ))}
        </Stack>
      </Stack>
    </Card>
  )
}

function DictionaryCard({
  dictionary,
  canAdminister,
  onEdit,
}: {
  dictionary: Dictionary
  canAdminister: boolean
  onEdit: () => void
}) {
  const t = useT()
  const remove = useDeleteDictionary()
  const importFile = useImportDictionary()
  const [confirmDelete, setConfirmDelete] = useState(false)
  const file = useRef<HTMLInputElement>(null)

  function report(err: unknown) {
    toast.error(t('hotwords.actionFailed'), {
      description: err instanceof ApiError ? err.message : String(err),
    })
  }

  return (
    <Card data-item={dictionary.key}>
      <Stack gap={3}>
        <Inline gap={2}>
          <span className="text-[13px] font-medium">{dictionary.name || dictionary.key}</span>
          <Badge>{dictionary.key}</Badge>
          <Badge>{t('hotwords.phrases', { count: dictionary.phrase_count })}</Badge>
          {dictionary.score !== undefined && dictionary.score > 0 && (
            <Badge>{t('hotwords.scoreIs', { score: dictionary.score })}</Badge>
          )}
        </Inline>

        {dictionary.description && (
          <p className="text-[12px] text-[var(--text-secondary)]">{dictionary.description}</p>
        )}

        {/* Why this dictionary came back from a search: the phrases that
            matched, not just the fact that something did. */}
        {dictionary.matches && dictionary.matches.length > 0 && (
          <Inline gap={2}>
            {dictionary.matches.map((m) => (
              <Badge key={m} tone="accent">
                {m}
              </Badge>
            ))}
          </Inline>
        )}

        <Inline gap={2}>
          <Button size="sm" onClick={onEdit}>
            {canAdminister ? t('hotwords.edit') : t('hotwords.view')}
          </Button>
          <Button
            size="sm"
            icon={<Download width={13} height={13} />}
            onClick={() => {
              void exportDictionary(dictionary.key)
                .then((text) => downloadText(`${dictionary.key}.txt`, text))
                .catch(report)
            }}
          >
            {t('hotwords.export')}
          </Button>
          {canAdminister && (
            <>
              <Button
                size="sm"
                busy={importFile.isPending}
                icon={<Upload width={13} height={13} />}
                onClick={() => file.current?.click()}
              >
                {t('hotwords.import')}
              </Button>
              <input
                ref={file}
                type="file"
                accept=".txt,.csv,.json,text/plain,text/csv,application/json"
                className="hidden"
                onChange={(e) => {
                  const chosen = e.target.files?.[0]
                  e.target.value = ''
                  if (!chosen) return
                  void importFile
                    .mutateAsync({ key: dictionary.key, file: chosen, mode: 'append' })
                    .then(() => toast.success(t('hotwords.imported')))
                    .catch(report)
                }}
              />
              <Button
                size="sm"
                variant="danger"
                icon={<Trash width={13} height={13} />}
                onClick={() => setConfirmDelete(true)}
              >
                {t('common.delete')}
              </Button>
            </>
          )}
        </Inline>
      </Stack>

      <Dialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={t('hotwords.confirmDelete')}
        description={t('hotwords.confirmDeleteBody')}
        footer={
          <>
            <Button onClick={() => setConfirmDelete(false)}>{t('common.cancel')}</Button>
            <Button
              variant="danger"
              onClick={() => {
                setConfirmDelete(false)
                void remove.mutateAsync(dictionary.key).catch(report)
              }}
            >
              {t('common.delete')}
            </Button>
          </>
        }
      >
        <p className="text-[13px]">{dictionary.name || dictionary.key}</p>
      </Dialog>
    </Card>
  )
}

/**
 * The editor, for a new dictionary and for an existing one.
 *
 * Phrases are edited as text, one per line, because that is how the list is
 * written, pasted and diffed everywhere else — including the file the import
 * endpoint reads.
 */
function DictionaryEditor({ editKey, onClose }: { editKey?: string; onClose: () => void }) {
  const t = useT()
  const auth = useAuth()
  const existing = useDictionary(editKey)
  const create = useCreateDictionary()
  const replace = useReplaceDictionary()

  const [draft, setDraft] = useState<{
    key: string
    name: string
    description: string
    score: string
    text: string
  }>()
  const [error, setError] = useState<ApiError>()

  const loaded = existing.data
  const value = draft ?? {
    key: loaded?.key ?? '',
    name: loaded?.name ?? '',
    description: loaded?.description ?? '',
    score: loaded?.score ? String(loaded.score) : '',
    text: (loaded?.phrases ?? []).join('\n'),
  }
  const set = (patch: Partial<typeof value>) => setDraft({ ...value, ...patch })

  const readOnly = auth.required && auth.key === ''
  const busy = create.isPending || replace.isPending
  const phraseCount = value.text.split('\n').filter((line) => line.trim() !== '').length

  function save() {
    setError(undefined)
    const input = {
      key: value.key,
      name: value.name,
      description: value.description,
      ...(value.score.trim() === '' ? {} : { score: Number(value.score) }),
      phrases: value.text.split('\n'),
    }
    const run = editKey === undefined ? create.mutateAsync(input) : replace.mutateAsync(input)
    void run
      .then(() => {
        toast.success(t('hotwords.saved'))
        onClose()
      })
      .catch((err: unknown) => {
        if (err instanceof ApiError) setError(err)
        else toast.error(String(err))
      })
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={editKey === undefined ? t('hotwords.new') : t('hotwords.editTitle')}
      footer={
        <>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
          {!readOnly && (
            <Button variant="primary" busy={busy} onClick={save}>
              {t('common.save')}
            </Button>
          )}
        </>
      }
    >
      {editKey !== undefined && existing.isLoading ? (
        <Skeleton className="h-40 w-full" />
      ) : (
        <Stack gap={4}>
          <Field
            label={t('hotwords.key')}
            description={t('hotwords.keyHint')}
            error={error?.param === 'key' ? error.message : undefined}
          >
            <Input
              value={value.key}
              spellCheck={false}
              disabled={editKey !== undefined || readOnly}
              onChange={(e) => set({ key: e.target.value })}
            />
          </Field>

          <Field label={t('hotwords.name')} error={error?.param === 'name' ? error.message : undefined}>
            <Input
              value={value.name}
              disabled={readOnly}
              onChange={(e) => set({ name: e.target.value })}
            />
          </Field>

          <Field label={t('hotwords.descriptionLabel')}>
            <Input
              value={value.description}
              disabled={readOnly}
              onChange={(e) => set({ description: e.target.value })}
            />
          </Field>

          <Field
            label={t('hotwords.score')}
            description={t('hotwords.scoreHint')}
            error={error?.param === 'score' ? error.message : undefined}
          >
            <Input
              type="number"
              min={0}
              max={10}
              step={0.1}
              value={value.score}
              disabled={readOnly}
              onChange={(e) => set({ score: e.target.value })}
            />
          </Field>

          <Field
            label={t('hotwords.phrasesLabel', { count: phraseCount })}
            description={t('hotwords.phrasesHint')}
            error={error?.param === 'phrases' ? error.message : undefined}
          >
            <Textarea
              rows={10}
              value={value.text}
              spellCheck={false}
              disabled={readOnly}
              onChange={(e) => set({ text: e.target.value })}
            />
          </Field>

          {error && error.param === undefined && (
            <p className="text-[12px] text-[var(--danger-text)]">{error.message}</p>
          )}
        </Stack>
      )}
    </Dialog>
  )
}

/** Hands the browser a file without a round trip through the server. */
function downloadText(filename: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
