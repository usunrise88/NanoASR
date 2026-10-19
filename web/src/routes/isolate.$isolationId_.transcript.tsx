import { useCallback, useMemo, useRef, useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Download, NavArrowLeft } from 'iconoir-react'

import { Card, Inline, Page, Section, Stack } from '@/components/layout'
import { Conversation } from '@/components/isolate/Conversation'
import { SpeakerShares } from '@/components/isolate/SpeakerShares'
import { VoiceBadge } from '@/components/isolate/VoiceName'
import { Player, type PlayerHandle } from '@/components/player/Player'
import { SpeakerTimeline, speakerLanes } from '@/components/player/SpeakerTimeline'
import { Badge, Button, EmptyState, ErrorState, Input, Skeleton } from '@/components/ui'
import { ApiError, speakerTranscriptText } from '@/lib/api/client'
import {
  useIsolation,
  useIsolationStream,
  useIsolationWav,
  useSpeakerTranscript,
  useUpdateIsolation,
} from '@/lib/api/hooks'
import type { Isolation, ResponseFormat, Result } from '@/lib/api/types'
import { recall } from '@/lib/audio-store'
import { conversation, shares, voiceName } from '@/lib/conversation'
import { saveBlob } from '@/lib/download'
import { clock } from '@/lib/format'
import { useT } from '@/lib/i18n'
import type { PageMeta } from '@/lib/page'
import { speakerIndex } from '@/lib/speakers'
import { toast } from '@/lib/toast'

export const pageMeta: PageMeta = {
  titleKey: 'isolate.transcriptTitle',
  descriptionKey: 'isolate.transcriptDescription',
}

export const Route = createFileRoute('/isolate/$isolationId_/transcript')({
  component: TranscriptPage,
  staticData: { pageMeta },
})

/**
 * The transcript by speaker, read as a conversation and listened to.
 *
 * The recording plays with a lane per voice under its waveform; any voice can
 * be listened to alone, from its track, and the position carries over. The
 * turns follow the playback, both voices' words lit where they overlap.
 */
function TranscriptPage() {
  const { isolationId } = Route.useParams()
  const t = useT()
  const navigate = useNavigate()
  const query = useIsolation(isolationId)
  const iso = query.data
  useIsolationStream(iso)
  const transcript = useSpeakerTranscript(iso)

  if (query.isLoading || (iso?.transcript?.status === 'succeeded' && transcript.isLoading)) {
    return (
      <Page>
        <Section>
          <Stack gap={3}>
            <Skeleton className="h-[120px] w-full" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-4/5" />
          </Stack>
        </Section>
      </Page>
    )
  }
  if (query.isError || !iso) {
    const gone = query.error instanceof ApiError && query.error.code === 'isolation_not_found'
    return (
      <Page>
        <Section>
          <ErrorState
            title={gone ? t('isolate.gone') : t('common.error')}
            detail={gone ? t('isolate.goneHint') : String(query.error)}
            action={
              <Button size="sm" onClick={() => void navigate({ to: '/isolate' })}>
                {t('isolate.again')}
              </Button>
            }
          />
        </Section>
      </Page>
    )
  }
  if (transcript.isError) {
    return (
      <Page>
        <Section>
          <ErrorState title={t('common.error')} detail={String(transcript.error)} />
        </Section>
      </Page>
    )
  }
  if (!transcript.data) {
    return (
      <Page>
        <Section>
          <EmptyState
            title={t('isolate.noTranscript')}
            description={t('isolate.noTranscriptHint')}
            action={
              <Button size="sm" onClick={() => void navigate({ to: '/isolate/$isolationId', params: { isolationId } })}>
                {t('isolate.backToVoices')}
              </Button>
            }
          />
        </Section>
      </Page>
    )
  }
  return <Reading iso={iso} result={transcript.data} />
}

function Reading({ iso, result }: { iso: Isolation; result: Result }) {
  const t = useT()
  const navigate = useNavigate()
  const update = useUpdateIsolation(iso.id)

  const talk = useMemo(() => conversation(result), [result])
  const voiceShares = useMemo(() => shares(result, talk.turns), [result, talk])
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set())
  const [search, setSearch] = useState('')

  // What is playing: the recording, or one voice's track.
  const [listen, setListen] = useState('')
  const [time, setTime] = useState(0)
  const playing = useRef(false)
  const [resume, setResume] = useState<{ at: number; play: boolean }>()
  const original = recall(iso.id)
  const track = iso.tracks?.find((tr) => tr.speaker === listen)
  const wav = useIsolationWav(iso.id, listen || undefined, track?.sample_start?.toString() ?? '', !(listen === '' && original))
  const blob = listen === '' && original ? original : wav.data
  const file = useMemo(
    () => (blob ? (blob instanceof File ? blob : new File([blob], `${listen || 'audio'}.wav`, { type: 'audio/wav' })) : undefined),
    [blob, listen],
  )

  const player = useRef<PlayerHandle>(null)
  const onReady = useCallback((handle: PlayerHandle) => {
    player.current = handle
  }, [])
  const seek = useCallback((seconds: number) => {
    player.current?.seek(seconds)
    setTime(seconds)
  }, [])

  const lanes = useMemo(() => speakerLanes(result, iso.voices), [result, iso.voices])

  function switchTo(speaker: string) {
    if (speaker === listen) return
    setResume({ at: time, play: playing.current })
    setListen(speaker)
  }

  async function rename(speaker: string, name: string) {
    try {
      await update.mutateAsync({ names: { [speaker]: name } })
    } catch (err) {
      toast.error(t('isolate.renameFailed'), { description: err instanceof Error ? err.message : String(err) })
      throw err
    }
  }

  function toggle(speaker: string) {
    setHidden((prev) => {
      const next = new Set(prev)
      if (next.has(speaker)) next.delete(speaker)
      else next.add(speaker)
      return next
    })
  }

  const tr = iso.transcript

  return (
    <Page>
      <Section>
        <Card>
          <Stack gap={3}>
            <Inline gap={3} className="justify-between">
              <Inline gap={3}>
                <Button
                  size="sm"
                  variant="ghost"
                  icon={<NavArrowLeft width={13} height={13} />}
                  onClick={() => void navigate({ to: '/isolate/$isolationId', params: { isolationId: iso.id } })}
                >
                  {t('isolate.backToVoices')}
                </Button>
                {iso.filename && <span className="truncate text-[13px] text-[var(--text-muted)]">{iso.filename}</span>}
                <span className="text-[13px] tabular-nums text-[var(--text-secondary)]">{clock(result.duration)}</span>
                <span className="text-[13px] text-[var(--text-secondary)]">{result.model}</span>
                {iso.keep && <Badge tone="accent">{t('isolate.kept')}</Badge>}
              </Inline>
              <Exports iso={iso} result={result} />
            </Inline>
            {tr?.stale && <p className="text-[12px] text-[var(--text-secondary)]">{t('isolate.transcriptStale')}</p>}

            {file ? (
              <Player
                key={listen}
                file={file}
                result={result}
                words={talk.words}
                currentTime={time}
                onTime={setTime}
                onReady={onReady}
                startAt={resume?.at}
                autoPlay={resume?.play}
                onPlaying={(p) => {
                  playing.current = p
                }}
                lanes={
                  <SpeakerTimeline
                    lanes={lanes}
                    duration={result.duration}
                    currentTime={time}
                    onSeek={seek}
                    dimmed={(s) => (listen !== '' && s !== listen) || hidden.has(s)}
                  />
                }
              />
            ) : wav.isError ? (
              <ErrorState title={t('isolate.audioFailed')} detail={String(wav.error)} />
            ) : (
              <Skeleton className="h-[120px] w-full" />
            )}

            <Inline gap={1}>
              <span className="mr-1 text-[12px] text-[var(--text-muted)]">{t('isolate.listen')}</span>
              <Button size="sm" variant={listen === '' ? 'primary' : 'ghost'} onClick={() => switchTo('')}>
                {t('isolate.listenAll')}
              </Button>
              {(iso.tracks ?? []).map((tk) => (
                <Button
                  key={tk.speaker}
                  size="sm"
                  variant={listen === tk.speaker ? 'primary' : 'ghost'}
                  icon={<VoiceBadge name={voiceName(iso.voices, tk.speaker)} index={speakerIndex(iso.voices, tk.speaker)} size="sm" />}
                  onClick={() => switchTo(tk.speaker)}
                >
                  {voiceName(iso.voices, tk.speaker)}
                </Button>
              ))}
            </Inline>
          </Stack>
        </Card>
      </Section>

      <Section title={t('isolate.speakers')} description={t('isolate.speakersHint')}>
        <SpeakerShares shares={voiceShares} voices={iso.voices} hidden={hidden} onToggle={toggle} onRename={rename} />
      </Section>

      <Section
        title={t('isolate.conversation')}
        actions={
          <Input
            className="w-56"
            value={search}
            placeholder={t('result.search')}
            onChange={(e) => setSearch(e.target.value)}
          />
        }
      >
        <Card>
          <Conversation talk={talk} voices={iso.voices} time={time} onSeek={seek} query={search} hidden={hidden} />
        </Card>
        {(result.warnings ?? []).map((w) => (
          <p key={w.code} className="mt-2 text-[12px] text-[var(--text-muted)]">
            {w.message}
          </p>
        ))}
      </Section>
    </Page>
  )
}

/**
 * Downloads, labelled by the voices' names. The text formats come from the
 * server, which renders them for the API too.
 */
function Exports({ iso, result }: { iso: Isolation; result: Result }) {
  const t = useT()
  const [busy, setBusy] = useState<ResponseFormat>()
  const base = (iso.filename ?? iso.id).replace(/\.[^.]+$/, '')

  async function save(format: ResponseFormat) {
    setBusy(format)
    try {
      const body =
        format === 'json' ? JSON.stringify(result, null, 2) : await speakerTranscriptText(iso.id, format)
      const ext = format === 'text' ? 'txt' : format
      saveBlob(new Blob([body], { type: 'text/plain;charset=utf-8' }), `${base}-transcript.${ext}`)
    } catch (err) {
      toast.error(t('common.error'), { description: String(err) })
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <Inline gap={1}>
      {(['text', 'srt', 'vtt', 'json'] as ResponseFormat[]).map((format) => (
        <Button
          key={format}
          size="sm"
          busy={busy === format}
          icon={<Download width={13} height={13} />}
          onClick={() => void save(format)}
        >
          {format === 'text' ? 'TXT' : format.toUpperCase()}
        </Button>
      ))}
    </Inline>
  )
}
