import { expect, test } from '@playwright/test'

/**
 * A diarized result reads as a conversation: turns by speaker, a lane per
 * speaker under the waveform, who spoke how much.
 *
 * Needs diarization switched on; without it the job comes back without
 * speakers and the suite skips. The clip is the two-voice fixture
 * scripts/fetch-testdata.sh derives.
 */
const CLIP = '../testdata/audio/ru-2spk.wav'

test('a diarized result reads as a conversation', async ({ page }) => {
  const fs = await import('node:fs')
  const submitted = await page.request.post('/api/v1/jobs', {
    multipart: {
      file: { name: 'ru-2spk.wav', mimeType: 'audio/wav', buffer: fs.readFileSync(CLIP) },
      diarize: 'true',
    },
  })
  expect(submitted.ok()).toBeTruthy()
  const { id } = (await submitted.json()) as { id: string }

  let speakers = 0
  await expect(async () => {
    const job = (await (await page.request.get(`/api/v1/jobs/${id}`)).json()) as {
      status: string
      result?: { speakers?: unknown[] }
    }
    expect(['succeeded', 'failed']).toContain(job.status)
    speakers = job.result?.speakers?.length ?? 0
  }).toPass({ timeout: 90_000 })
  test.skip(speakers < 2, 'diarization is not enabled on this server')

  await page.goto(`/ui/result/${id}`)
  // A reload has no audio: the page takes the file again, and plays it.
  await page.setInputFiles('input[type=file]', CLIP)

  await expect(page.locator('[data-lane]')).toHaveCount(speakers)
  await expect(page.locator('[data-share]')).toHaveCount(speakers)
  await expect(page.locator('[data-turn]').first()).toBeVisible()

  await page.locator('[data-word]').first().click()
  await expect(page.locator('[data-current]').first()).toBeVisible()

  // A speaker's card hides that speaker's turns.
  const first = (await page.locator('[data-turn]').first().getAttribute('data-turn')) ?? ''
  await page.locator(`[data-share="${first}"]`).getByRole('button', { name: /^(Hide|Скрыть)$/ }).click()
  await expect(page.locator(`[data-turn="${first}"]`)).toHaveCount(0)
})
