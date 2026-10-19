import { useState } from 'react'
import { Play } from 'iconoir-react'

import { Stack } from '@/components/layout'
import { ModelPicker } from '@/components/run/ModelPicker'
import { RunOptions } from '@/components/run/Options'
import { Button, Sheet } from '@/components/ui'
import { useCatalog, useModels } from '@/lib/api/hooks'
import type { TranscribeOptions } from '@/lib/api/types'

/**
 * The run screen's model and options, for audio the isolation already holds:
 * one voice's track, or every track for the transcript by speaker. Each track
 * is one voice, so diarization and the channel mode are not offered.
 */
export function TranscribeSheet({
  open,
  onOpenChange,
  title,
  description,
  submitLabel,
  busy,
  onRun,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: string
  submitLabel: string
  busy: boolean
  onRun: (options: TranscribeOptions) => void
}) {
  const models = useModels()
  const catalog = useCatalog()
  const [modelID, setModelID] = useState('')
  const [options, setOptions] = useState<TranscribeOptions>({})

  const installed = models.data ?? []
  const effectiveModel = modelID || installed.find((m) => m.kind === '' || m.kind === 'asr')?.id || ''
  const chosen = installed.find((m) => m.id === effectiveModel)

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      description={description}
      footer={
        <Button
          variant="primary"
          icon={<Play width={14} height={14} />}
          busy={busy}
          disabled={!effectiveModel}
          onClick={() => onRun(effectiveModel ? { ...options, model: effectiveModel } : options)}
        >
          {submitLabel}
        </Button>
      }
    >
      <Stack gap={4}>
        {(installed.length > 0 || (catalog.data?.length ?? 0) > 0) && (
          <ModelPicker
            models={installed}
            catalog={catalog.data ?? []}
            value={effectiveModel}
            onChange={setModelID}
            disabled={busy}
          />
        )}
        <RunOptions value={options} onChange={setOptions} model={chosen} disabled={busy} oneVoice />
      </Stack>
    </Sheet>
  )
}
