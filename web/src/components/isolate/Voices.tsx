import { useState } from 'react'
import { Check, UserCircle } from 'iconoir-react'

import { Inline, Stack } from '@/components/layout'
import { AudioClip } from '@/components/player/AudioClip'
import { Badge, Button, Input } from '@/components/ui'
import { fetchVoiceSample } from '@/lib/api/client'
import type { Isolation, IsolationSelection, IsolationVoice } from '@/lib/api/types'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'

/**
 * The voices found in a recording, each with its sample to listen to and a
 * button to keep it.
 *
 * The sample is exactly the 3 seconds the extractor will be told to keep, so
 * what a person hears here is what decides the result. When it is the wrong
 * voice — a hand-over the diarizer placed late — they can name another place
 * in the recording to take it from.
 */
export function Voices({
  iso,
  busy,
  onChoose,
}: {
  iso: Isolation
  busy: boolean
  onChoose: (selection: IsolationSelection) => void
}) {
  return (
    <Stack gap={2}>
      {(iso.voices ?? []).map((voice) => (
        <VoiceRow
          key={voice.speaker}
          iso={iso}
          voice={voice}
          busy={busy}
          chosen={iso.result?.speaker === voice.speaker}
          onChoose={onChoose}
        />
      ))}
    </Stack>
  )
}

function VoiceRow({
  iso,
  voice,
  busy,
  chosen,
  onChoose,
}: {
  iso: Isolation
  voice: IsolationVoice
  busy: boolean
  chosen: boolean
  onChoose: (selection: IsolationSelection) => void
}) {
  const t = useT()
  const [placing, setPlacing] = useState(!voice.sample)
  const [start, setStart] = useState(voice.sample?.start !== undefined ? String(voice.sample.start) : '')
  const maxStart = Math.max(0, (iso.duration ?? 3) - 3)
  const parsed = Number.parseFloat(start)
  const validStart = Number.isFinite(parsed) && parsed >= 0 && parsed <= maxStart

  return (
    <div
      className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] px-3 py-2.5"
      data-voice={voice.speaker}
    >
      <Stack gap={2}>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Inline gap={2}>
            <UserCircle width={16} height={16} className="text-[var(--text-secondary)]" />
            <span className="text-[13px] font-medium">{voice.speaker}</span>
            <span className="text-[12px] text-[var(--text-secondary)]">
              {t('isolate.speech', { time: clock(voice.total_speech), turns: voice.turns })}
            </span>
            {chosen && <Badge tone="success">{t('isolate.chosen')}</Badge>}
          </Inline>
          <Inline gap={1}>
            {voice.sample && !placing && (
              <Button variant="ghost" size="sm" disabled={busy} onClick={() => setPlacing(true)}>
                {t('isolate.otherSample')}
              </Button>
            )}
            <Button
              variant={chosen ? 'secondary' : 'primary'}
              size="sm"
              icon={<Check width={13} height={13} />}
              disabled={busy || (placing ? !validStart : !voice.sample)}
              onClick={() =>
                onChoose(placing ? { speaker: voice.speaker, sample_start: parsed } : { speaker: voice.speaker })
              }
            >
              {t('isolate.keep')}
            </Button>
          </Inline>
        </div>

        {voice.sample ? (
          <AudioClip
            label={
              voice.sample.kind === 'window'
                ? t('isolate.sampleAt', { start: clock(voice.sample.start ?? 0) })
                : t('isolate.sampleAssembled')
            }
            load={() => fetchVoiceSample(iso.id, voice.speaker)}
          />
        ) : (
          <p className="text-[12px] text-[var(--text-secondary)]">
            {t('isolate.noSample', { reason: voice.reason ?? '' })}
          </p>
        )}

        {placing && (
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-[12px] text-[var(--text-secondary)]">{t('isolate.sampleFrom')}</span>
            <Input
              type="number"
              min={0}
              max={maxStart}
              step={0.5}
              inputMode="decimal"
              className="w-24"
              aria-label={t('isolate.sampleFrom')}
              value={start}
              onChange={(e) => setStart(e.target.value)}
            />
            <span className="text-[12px] text-[var(--text-muted)]">
              {t('isolate.sampleFromHint', { max: clock(maxStart) })}
            </span>
            {voice.sample && (
              <Button variant="ghost" size="sm" onClick={() => setPlacing(false)}>
                {t('common.cancel')}
              </Button>
            )}
          </div>
        )}
      </Stack>
    </div>
  )
}
