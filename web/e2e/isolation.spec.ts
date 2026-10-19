import { expect, test, type Page } from '@playwright/test'

/**
 * Voice isolation, in a browser: upload, listen to the voices, keep one,
 * download it, transcribe it.
 *
 * Needs isolation switched on (isolation.enabled with diarization.enabled) and
 * its models pulled; without them the suite skips. The clip is the two-voice
 * fixture scripts/fetch-testdata.sh derives: one voice, then the same clip
 * pitched into a second one.
 */
const CLIP = '../testdata/audio/ru-2spk.wav'

test.beforeEach(async ({ page }) => {
  const info = await page.request.get('/api/v1/isolation')
  const enabled = ((await info.json()) as { enabled: boolean }).enabled
  test.skip(!enabled, 'voice isolation is not enabled on this server')

  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(String(e)))
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text())
  })
  ;(page as Page & { __errors?: string[] }).__errors = errors
})

test.afterEach(async ({ page }) => {
  const p = page as Page & { __errors?: string[]; __expect404?: boolean }
  // The browser logs a refused request to the console itself; where the 404
  // is the point of the test, that line is expected.
  const errors = (p.__errors ?? []).filter((e) => !(p.__expect404 && e.includes('404')))
  expect(errors, `console errors: ${errors.join(' | ')}`).toHaveLength(0)
})

test('find the voices, keep one, download it and transcribe it', async ({ page }) => {
  await page.goto('/ui/')
  // Offered in the header only where the server can do it.
  await page.getByRole('link', { name: /^(Voice isolation|Выделение голоса)$/ }).click()
  await page.waitForURL(/\/ui\/isolate\/?$/)

  await page.setInputFiles('input[type=file]', CLIP)
  await page.getByRole('button', { name: /^(Find the voices|Найти голоса)$/ }).click()
  await page.waitForURL(/\/ui\/isolate\/iso_/)

  // Both voices, each with a sample to listen to.
  const voices = page.locator('[data-voice]')
  await expect(voices).toHaveCount(2)
  const first = voices.first()
  await first.getByRole('button', { name: /^(Play|Воспроизвести)$/ }).click()
  // The sample is fetched with the key and played: its length is 3 seconds.
  await expect(first.getByText(/\/ 0:03$/)).toBeVisible()

  await first.getByRole('button', { name: /^(Keep this voice|Оставить этот голос)$/ }).click()
  await expect(page.getByText(/^(Isolated voice|Выделенный голос)$/)).toBeVisible()
  await expect(first.getByText(/^(Isolated|Выделен)$/)).toBeVisible()

  // The isolated voice plays beside the original the browser still holds.
  await expect(page.locator('[data-clip]')).toHaveCount(4)

  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: /^(Download|Скачать) MP3$/ }).click()
  expect((await download).suggestedFilename()).toMatch(/^ru-2spk-spk_\d\.mp3$/)

  await page.getByRole('button', { name: /^(Transcribe|Расшифровать)$/ }).click()
  await page.waitForURL(/\/ui\/result\//)
  await expect(page.locator('[data-word="0"]')).toBeVisible()
})

test('an isolation that is gone says so and offers to start again', async ({ page }) => {
  ;(page as Page & { __expect404?: boolean }).__expect404 = true
  await page.goto('/ui/isolate/iso_000000000000000000000000')
  await expect(page.getByText(/(no longer on the server|уже нет на сервере)/)).toBeVisible()
  await page.getByRole('button', { name: /^(Upload again|Загрузить заново)$/ }).click()
  await page.waitForURL(/\/ui\/isolate\/?$/)
})
