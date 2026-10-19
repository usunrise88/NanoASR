import { expect, test, type Page } from '@playwright/test'

/**
 * Voice isolation, in a browser: upload, listen to the voices, keep one,
 * download it, transcribe it; or name them all, isolate them all and read the
 * conversation by speaker.
 *
 * Needs isolation switched on (isolation.enabled with diarization.enabled) and
 * its models pulled; without them the suite skips. The clip is the two-voice
 * fixture scripts/fetch-testdata.sh derives: one voice, then the same clip
 * pitched into a second one.
 */
const CLIP = '../testdata/audio/ru-2spk.wav'
const FIND = /^(Find the voices|Найти голоса)$/

/**
 * Picks the clip on the isolation page. The URL changes before the route's
 * chunk has loaded, and until it has the page on screen is the one before —
 * which has a file input of its own. Waiting for this page's own button puts
 * the file where this page will read it.
 */
async function chooseFile(page: Page) {
  await expect(page.getByRole('button', { name: FIND })).toBeVisible()
  await page.setInputFiles('input[type=file]', CLIP)
  await expect(page.getByRole('button', { name: FIND })).toBeEnabled()
}

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

  await chooseFile(page)
  await page.getByRole('button', { name: FIND }).click()
  await page.waitForURL(/\/ui\/isolate\/iso_/)

  // Both voices, each with a sample to listen to.
  const voices = page.locator('[data-voice]')
  await expect(voices).toHaveCount(2)
  const first = voices.first()
  await first.getByRole('button', { name: /^(Play|Воспроизвести)$/ }).click()
  // The sample is fetched with the key and played: its length is 3 seconds.
  await expect(first.getByText(/\/ 0:03$/)).toBeVisible()

  await first.getByRole('button', { name: /^(Isolate this voice|Выделить этот голос)$/ }).click()
  await expect(first.locator('[data-track]')).toBeVisible()
  await expect(first.getByText(/^(Isolated|Выделен)$/)).toBeVisible()

  // The original the browser still holds, two samples and the track.
  await expect(page.locator('[data-clip]')).toHaveCount(4)

  const download = page.waitForEvent('download')
  await first.getByRole('button', { name: /^(Download|Скачать) MP3$/ }).click()
  expect((await download).suggestedFilename()).toMatch(/^ru-2spk-spk_\d\.mp3$/)

  await first.getByRole('button', { name: /^(Transcribe the track|Расшифровать дорожку)$/ }).click()
  await page.getByRole('button', { name: /^(Transcribe|Расшифровать)$/ }).click()
  await page.waitForURL(/\/ui\/result\//)
  await expect(page.locator('[data-word="0"]')).toBeVisible()
})

test('name the voices, isolate them all and read the conversation by speaker', async ({ page }) => {
  await page.goto('/ui/isolate')
  await chooseFile(page)
  // Kept: it is listed, and it would survive a restart.
  await page.getByRole('switch').click()
  await page.getByRole('button', { name: FIND }).click()
  await page.waitForURL(/\/ui\/isolate\/iso_/)
  const id = /iso_[0-9a-f]+/.exec(page.url())?.[0] ?? ''

  const voices = page.locator('[data-voice]')
  await expect(voices).toHaveCount(2)
  const first = voices.first()
  const speaker = (await first.getAttribute('data-voice')) ?? ''
  await first.getByRole('button', { name: /^(Rename|Переименовать)$/ }).click()
  // A Latin name: a browser without a UTF-8 locale saves any other name as
  // "download", and that is the browser's doing, not this page's.
  await first.getByRole('textbox').fill('Anna')
  await first.getByRole('textbox').press('Enter')
  await expect(first.locator('[data-voice-name]')).toHaveText('Anna')

  await page.getByRole('button', { name: /^(Isolate all|Выделить всех)$/ }).click()
  await expect(page.locator('[data-track]')).toHaveCount(2)
  const track = page.waitForEvent('download')
  await first.getByRole('button', { name: /^(Download|Скачать) WAV$/ }).click()
  expect((await track).suggestedFilename()).toBe('ru-2spk-Anna.wav')

  await page.getByRole('button', { name: /^(Transcribe by speaker|Распознать по спикерам)$/ }).click()
  await page.getByRole('button', { name: /^(Transcribe|Распознать)$/ }).click()
  await page.getByRole('button', { name: /^(Open the transcript|Открыть транскрипт)$/ }).click()
  await page.waitForURL(new RegExp(`/ui/isolate/${id}/transcript$`))

  // A turn per voice, in its name; a lane per voice under the waveform.
  await expect(page.locator(`[data-turn="${speaker}"]`).first()).toContainText('Anna')
  await expect(page.locator('[data-lane]')).toHaveCount(2)
  await page.locator('[data-word]').first().click()
  await expect(page.locator('[data-current]').first()).toBeVisible()

  const text = page.waitForEvent('download')
  await page.getByRole('button', { name: /^TXT$/ }).click()
  const saved = await text
  expect(saved.suggestedFilename()).toBe('ru-2spk-transcript.txt')
  const path = await saved.path()
  const fs = await import('node:fs')
  expect(fs.readFileSync(path, 'utf8')).toMatch(/^Anna: |\n\nAnna: /)

  // Listed as kept on the upload page, and deleted from there for good.
  await page.goto('/ui/isolate')
  await expect(page.locator(`[data-isolation="${id}"]`)).toBeVisible()
  await page.request.delete(`/api/v1/isolations/${id}`)
})

test('an isolation that is gone says so and offers to start again', async ({ page }) => {
  ;(page as Page & { __expect404?: boolean }).__expect404 = true
  await page.goto('/ui/isolate/iso_000000000000000000000000')
  await expect(page.getByText(/(no longer on the server|уже нет на сервере)/)).toBeVisible()
  await page.getByRole('button', { name: /^(Upload again|Загрузить заново)$/ }).click()
  await page.waitForURL(/\/ui\/isolate\/?$/)
})
