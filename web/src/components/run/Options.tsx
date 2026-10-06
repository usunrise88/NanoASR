import { Inline, Stack } from '@/components/layout'
import { Button, Field, Input, Select, Switch } from '@/components/ui'
import { useDictionaryList } from '@/lib/api/hooks'
import type { ModelInfo, TranscribeOptions } from '@/lib/api/types'
import { useT } from '@/lib/i18n'

/**
 * The request parameters, with what this model cannot do turned off.
 *
 * The server would answer with a warning either way — that is what
 * `capabilities` and the warning list are for — but learning before the run
 * costs nothing, and an option that silently did nothing would be worse than an
 * option that says why it is unavailable.
 */
export function RunOptions({
  value,
  onChange,
  model,
  disabled,
}: {
  value: TranscribeOptions
  onChange: (next: TranscribeOptions) => void
  model: ModelInfo | undefined
  disabled?: boolean | undefined
}) {
  const t = useT()
  const set = <K extends keyof TranscribeOptions>(key: K, v: TranscribeOptions[K]) =>
    onChange({ ...value, [key]: v })

  // What the server cannot do is switched off here rather than left on to
  // produce a warning nobody reads. Punctuation is the model's own capability,
  // so it follows the manifest: a model that writes its own marks needs no
  // option, and one that does not cannot be made to.
  const canPunctuate = model?.capabilities.punctuation_builtin ?? false
  // Whether a bias list will reach the decoder at all, and why not. The server
  // answers both per model; repeating the reason here is what stops somebody
  // typing a vocabulary into a field that is going to ignore it.
  const canBias = model?.capabilities.hotwords ?? true
  const biasReason = model?.capabilities.hotwords_reason ?? t('home.unsupported')
  const dictionaries = useDictionaryList()

  return (
    <Stack gap={4}>
      <Field label={t('home.language')}>
        <Input
          value={value.language ?? ''}
          disabled={disabled}
          placeholder={t('home.languageAuto')}
          spellCheck={false}
          onChange={(e) => set('language', e.target.value)}
        />
      </Field>

      <Field label={t('home.channelMode')}>
        <Select
          value={value.channel_mode ?? 'downmix'}
          disabled={disabled}
          onChange={(v) => set('channel_mode', v as TranscribeOptions['channel_mode'])}
          options={[
            { value: 'downmix', label: 'downmix' },
            { value: 'first', label: 'first' },
            { value: 'split', label: 'split' },
          ]}
        />
      </Field>

      <Field label={t('home.decoding')}>
        <Select
          value={value.decoding_method ?? 'greedy_search'}
          disabled={disabled}
          onChange={(v) => set('decoding_method', v as TranscribeOptions['decoding_method'])}
          options={[
            { value: 'greedy_search', label: 'greedy_search' },
            { value: 'modified_beam_search', label: 'modified_beam_search' },
          ]}
        />
      </Field>

      {/* Biasing is a property of the model, like punctuation above: a model
          without the machinery cannot be made to have it, so the fields stay
          visible — the list is still worth writing — and say what will happen
          to them. */}
      <Field
        label={t('home.hotwords')}
        description={canBias ? t('home.hotwordsHint') : biasReason}
      >
        <Input
          value={(value.hotwords ?? []).join(', ')}
          disabled={disabled}
          spellCheck={false}
          onChange={(e) =>
            set(
              'hotwords',
              e.target.value
                .split(',')
                .map((w) => w.trim())
                .filter(Boolean),
            )
          }
        />
      </Field>

      {/* A group of toggles rather than a Field: there is no single control for
          a label to point at, and a label pointing at nothing is worse than a
          heading that says what the group is. */}
      {dictionaries.length > 0 && (
        <Stack gap={1}>
          <span className="text-[13px] font-medium">{t('home.dictionaries')}</span>
          <Inline gap={2}>
            {dictionaries.map((d) => {
              const picked = (value.hotwords_dict ?? []).includes(d.key)
              return (
                <Button
                  key={d.key}
                  size="sm"
                  variant={picked ? 'primary' : 'secondary'}
                  disabled={disabled ?? false}
                  onClick={() =>
                    set(
                      'hotwords_dict',
                      picked
                        ? (value.hotwords_dict ?? []).filter((k) => k !== d.key)
                        : [...(value.hotwords_dict ?? []), d.key],
                    )
                  }
                >
                  {d.name || d.key}
                </Button>
              )
            })}
          </Inline>
          <span className="text-[12px] text-[var(--text-muted)]">
            {t('home.dictionariesHint')}
          </span>
        </Stack>
      )}

      <Stack gap={3}>
        <Switch
          label={t('home.punctuate')}
          checked={canPunctuate && (value.punctuate ?? false)}
          disabled={(disabled ?? false) || !canPunctuate}
          description={canPunctuate ? t('home.punctuateHint') : t('home.unsupported')}
          onChange={(v) => set('punctuate', v)}
        />
        <Switch
          label={t('home.itn')}
          checked={value.itn ?? false}
          disabled={disabled ?? false}
          description={t('home.itnHint')}
          onChange={(v) => set('itn', v)}
        />
        <Switch
          label={t('home.diarize')}
          checked={value.diarize ?? false}
          disabled={disabled ?? false}
          description={t('home.diarizeHint')}
          onChange={(v) => set('diarize', v)}
        />
        {(value.diarize ?? false) && (
          <Field label={t('home.numSpeakers')} description={t('home.numSpeakersHint')}>
            <Input
              type="number"
              min={0}
              max={20}
              value={String(value.num_speakers ?? 0)}
              disabled={disabled ?? false}
              onChange={(e) => set('num_speakers', Number(e.target.value) || 0)}
            />
          </Field>
        )}
      </Stack>
    </Stack>
  )
}
