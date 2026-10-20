import { useState } from 'react'
import { Check, Download, Page } from 'iconoir-react'

import { Inline, Stack } from '@/components/layout'
import { AudioClip } from '@/components/player/AudioClip'
import { Badge, Button, Input } from '@/components/ui'
import { fetchTrack, fetchVoiceSample } from '@/lib/api/client'
import type {
  AudioFormat,
  Isolation,
  IsolationMedia,
  IsolationResult,
  IsolationSelection,
  IsolationVoice,
} from '@/lib/api/types'
import { saveBlob } from '@/lib/download'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'
import { toast } from '@/lib/toast'

import { VoiceName } from './VoiceName'

/**
 * The voices found in a recording: each with its name, its sample to listen
 * to, a button to isolate it, and once isolated, its track.
 *
 * The sample is exactly the 3 seconds the extractor will be told to keep, so
 * what a person hears here is what decides the result. When it is the wrong
 * voice — a hand-over the diarizer placed late — they can name another place
 * in the recording to take it from.
 */
export function Voices({
  iso,
  media,
  busy,
  onChoose,
  onRename,
  onTranscribe,
}: {
  iso: Isolation
  // media is where the samples and tracks stream from; until it is known
  // they are fetched whole.
  media: IsolationMedia | undefined
  busy: boolean
  onChoose: (selection: IsolationSelection) => void
  onRename: (speaker: string, name: string) => Promise<void>
  onTranscribe: (speaker: string) => void
}) {
  return (
    <Stack gap={2}>
      {(iso.voices ?? []).map((voice, index) => (
        <VoiceRow
          key={voice.speaker}
          iso={iso}
          media={media}
          voice={voice}
          index={index}
          track={iso.tracks?.find((t) => t.speaker === voice.speaker)}
          working={iso.voice === voice.speaker}
          busy={busy}
          onChoose={onChoose}
          onRename={(name) => onRename(voice.speaker, name)}
          onTranscribe={() => onTranscribe(voice.speaker)}
        />
      ))}
    </Stack>
  )
}

function VoiceRow({
  iso,
  media,
  voice,
  index,
  track,
  working,
  busy,
  onChoose,
  onRename,
  onTranscribe,
}: {
  iso: Isolation
  media: IsolationMedia | undefined
  voice: IsolationVoice
  index: number
  track: IsolationResult | undefined
  working: boolean
  busy: boolean
  onChoose: (selection: IsolationSelection) => void
  onRename: (name: string) => Promise<void>
  onTranscribe: () => void
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
            <VoiceName speaker={voice.speaker} name={voice.name} index={index} onRename={onRename} />
            <span className="text-[12px] text-[var(--text-secondary)]">
              {t('isolate.speech', { time: clock(voice.total_speech), turns: voice.turns })}
            </span>
            {track && <Badge tone="success">{t('isolate.chosen')}</Badge>}
            {working && <Badge tone="accent">{t('isolate.working')}</Badge>}
          </Inline>
          <Inline gap={1}>
            {voice.sample && !placing && (
              <Button variant="ghost" size="sm" disabled={busy} onClick={() => setPlacing(true)}>
                {t('isolate.otherSample')}
              </Button>
            )}
            <Button
              variant={track ? 'secondary' : 'primary'}
              size="sm"
              icon={<Check width={13} height={13} />}
              disabled={busy || (placing ? !validStart : !voice.sample)}
              onClick={() =>
                onChoose(placing ? { speaker: voice.speaker, sample_start: parsed } : { speaker: voice.speaker })
              }
            >
              {track ? t('isolate.reisolate') : t('isolate.keep')}
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
            src={media?.samples[voice.speaker]}
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

        {track && (
          <Track
            iso={iso}
            src={media?.tracks[track.speaker]}
            track={track}
            label={t('isolate.isolated', { speaker: voice.name || voice.speaker })}
            busy={busy}
            onTranscribe={onTranscribe}
          />
        )}
      </Stack>
    </div>
  )
}

/** One voice's track: to listen to, to download, to transcribe alone. */
function Track({
  iso,
  src,
  track,
  label,
  busy,
  onTranscribe,
}: {
  iso: Isolation
  src: string | undefined
  track: IsolationResult
  label: string
  busy: boolean
  onTranscribe: () => void
}) {
  const t = useT()
  const [downloading, setDownloading] = useState<AudioFormat>()

  async function download(format: AudioFormat) {
    setDownloading(format)
    try {
      saveBlob(await fetchTrack(iso.id, track.speaker, format), `${track.name}.${format}`)
    } catch (err) {
      toast.error(t('isolate.downloadFailed'), { description: err instanceof Error ? err.message : String(err) })
    } finally {
      setDownloading(undefined)
    }
  }

  return (
    <div className="rounded-[var(--radius-md)] bg-[var(--bg-raised)] px-2.5 py-2" data-track={track.speaker}>
      <Stack gap={2}>
        <AudioClip label={label} src={src} load={() => fetchTrack(iso.id, track.speaker, 'wav')} />
        <Inline gap={1}>
          {(iso.formats ?? ['wav']).map((format) => (
            <Button
              key={format}
              size="sm"
              variant="ghost"
              icon={<Download width={13} height={13} />}
              busy={downloading === format}
              disabled={downloading !== undefined}
              onClick={() => void download(format)}
            >
              {t('isolate.download', { format: format.toUpperCase() })}
            </Button>
          ))}
          <Button size="sm" variant="ghost" icon={<Page width={13} height={13} />} disabled={busy} onClick={onTranscribe}>
            {t('isolate.transcribeTrack')}
          </Button>
        </Inline>
      </Stack>
    </div>
  )
}
