import { Stack } from '@/components/layout'
import { Field, Input, Select, Switch } from '@/components/ui'
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

      <Field label={t('home.hotwords')} description={t('home.hotwordsHint')}>
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
