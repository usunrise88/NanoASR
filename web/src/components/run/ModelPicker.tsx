import { Download } from 'iconoir-react'

import { Stack } from '@/components/layout'
import { Badge, Button, Field, Progress, Select } from '@/components/ui'
import type { ModelInfo } from '@/lib/api/types'
import { useModelDownload } from '@/lib/api/hooks'
import { modelStateKey, modelTone } from '@/lib/format'
import { useT } from '@/lib/i18n'

/**
 * Choosing a model, and getting one that is not here yet.
 *
 * The picker lists what is on disk and what the catalog offers in one control,
 * because to the person choosing they are the same decision — the difference is
 * only whether pressing the button costs a download first.
 */
export function ModelPicker({
  models,
  catalog,
  value,
  onChange,
  disabled,
}: {
  models: ModelInfo[]
  catalog: ModelInfo[]
  value: string
  onChange: (id: string) => void
  disabled?: boolean | undefined
}) {
  const t = useT()
  const download = useModelDownload()

  const installed = models.filter(isTranscription)
  const onDisk = new Set(installed.map((m) => m.id))
  const missing = catalog.filter((m) => isTranscription(m) && !onDisk.has(m.id))

  const selected = installed.find((m) => m.id === value) ?? missing.find((m) => m.id === value)
  const needsDownload = selected !== undefined && !onDisk.has(selected.id)

  return (
    <Field label={t('home.model')}>
      <Stack gap={2}>
        <Select
          value={value}
          disabled={disabled}
          onChange={onChange}
          options={[
            ...installed.map((m) => ({ value: m.id, label: label(m) })),
            ...missing.map((m) => ({ value: m.id, label: `${label(m)} — ${t('home.modelAbsent')}` })),
          ]}
        />

        {selected && (
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={modelTone(selected.state)}>{t(modelStateKey(selected.state))}</Badge>
            {selected.languages.map((l) => (
              <Badge key={l}>{l}</Badge>
            ))}
            {selected.capabilities.word_timestamps && <Badge tone="accent">words</Badge>}
            {selected.license && (
              <span
                title={selected.license}
                className="truncate text-[11px] text-[var(--text-muted)]"
              >
                {licenceLabel(selected.license)}
              </span>
            )}
          </div>
        )}

        {needsDownload && !download.running && (
          <Button
            size="sm"
            icon={<Download width={14} height={14} />}
            onClick={() => download.start(selected.id)}
            disabled={disabled}
          >
            {t('home.download')}
          </Button>
        )}

        {download.running && download.progress && (
          <Progress percent={download.progress.percent} label={t('home.download')} />
        )}
        {download.error && (
          <p className="text-[12px] text-[var(--danger-text)]">{download.error}</p>
        )}
      </Stack>
    </Field>
  )
}

/** Supporting models — VAD, punctuation — are not things to transcribe with. */
function isTranscription(m: ModelInfo): boolean {
  return m.kind === '' || m.kind === 'asr'
}

function label(m: ModelInfo): string {
  return m.display_name || m.id
}

/**
 * A licence is often a URL to the terms rather than an SPDX identifier, and a
 * full GitHub link crowds the row out. The whole value stays in the tooltip,
 * because which licence weights carry is not a detail to hide.
 */
function licenceLabel(licence: string): string {
  if (!/^https?:\/\//.test(licence)) return licence
  try {
    const url = new URL(licence)
    return `${url.hostname.replace(/^www\./, '')}/…`
  } catch {
    return licence
  }
}
