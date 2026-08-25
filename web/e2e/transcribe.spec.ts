import { expect, test, type Page } from '@playwright/test'

/**
 * The whole point of the product, in a browser: pick a file, transcribe it,
 * hear it back with the transcript following along.
 *
 * Everything here goes through the real server and real weights. The unit tests
 * cover the arithmetic; this covers the parts only a browser has — a worker, a
 * canvas, an audio element and an SSE stream.
 */
const CLIP = '../testdata/audio/ru-16k.wav'

test.beforeEach(async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(String(e)))
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text())
  })
  // A screen that works but logs a stack trace is not a screen that works.
  test.info().annotations.push({ type: 'console', description: 'collected' })
  await page.goto('/ui/')
  ;(page as Page & { __errors?: string[] }).__errors = errors
})

test.afterEach(async ({ page }) => {
  const errors = (page as Page & { __errors?: string[] }).__errors ?? []
  expect(errors, `console errors: ${errors.join(' | ')}`).toHaveLength(0)
})

async function transcribe(page: Page) {
  await page.setInputFiles('input[type=file]', CLIP)
  await page.getByRole('button', { name: /^(Transcribe|Распознать)$/ }).click()
  await page.waitForURL(/\/ui\/result\//)
  await expect(page.locator('[data-word="0"]')).toBeVisible()
}

test('transcribes a file and plays it back word by word', async ({ page }) => {
  await transcribe(page)

  // Words came back with timings, not just text.
  const words = await page.locator('[data-word]').count()
  expect(words).toBeGreaterThan(5)

  // The waveform is built in a worker from the local file: the server never
  // sends audio back, so ink on the canvas proves the whole local path.
  await expect
    .poll(() => page.evaluate(() => (document.querySelector('canvas')?.width ?? 0) > 0))
    .toBe(true)
  const painted = await page.evaluate(() => {
    const canvas = document.querySelector('canvas')
    if (!canvas) return 0
    const data = canvas.getContext('2d')?.getImageData(0, 0, canvas.width, canvas.height).data
    if (!data) return 0
    let ink = 0
    for (let i = 3; i < data.length; i += 4) if ((data[i] ?? 0) > 0) ink++
    return ink
  })
  expect(painted).toBeGreaterThan(1000)

  // Clicking a word seeks to that word and highlights it. The seek lands on
  // the audio element's sample grid, a hair before the word's own start, which
  // is exactly the case wordAt's tolerance exists for.
  const word = page.locator('[data-word="3"]')
  const start = Number((await word.getAttribute('title'))?.replace('s', ''))
  await word.click()

  const at = await page.evaluate(() => document.querySelector('audio')?.currentTime ?? -1)
  expect(Math.abs(at - start)).toBeLessThan(0.05)
  await expect(page.locator('[data-word="3"][data-current]')).toHaveCount(1)
})

test('shows the transcript without a player after a reload', async ({ page }) => {
  await transcribe(page)
  const url = page.url()

  // Audio never reaches the server, so a reload cannot get it back. The page
  // is expected to say so and keep the transcript.
  await page.reload()
  await expect(page.locator('[data-word="0"]')).toBeVisible()
  await expect(page.locator('canvas')).toHaveCount(0)
  await expect(page.getByText(/not in this tab|нет в этой вкладке/i)).toBeVisible()

  // Attaching the same file again restores the player.
  await page.setInputFiles('input[type=file]', CLIP)
  await expect
    .poll(() => page.evaluate(() => (document.querySelector('canvas')?.width ?? 0) > 0))
    .toBe(true)
  expect(page.url()).toBe(url)
})

test('lists the finished job in the history', async ({ page }) => {
  await transcribe(page)
  const jobId = new URL(page.url()).pathname.split('/').pop() ?? ''

  await page.goto('/ui/jobs')
  const row = page.locator(`a[href$="${jobId}"]`)
  await expect(row).toBeVisible()
  await row.click()
  await expect(page).toHaveURL(new RegExp(jobId))
})

test('reports models by what is actually true of them', async ({ page }) => {
  await page.goto('/ui/models')

  // A model on disk is "On disk" until it is loaded, never "Not downloaded" —
  // the two were conflated by the server until this milestone.
  await expect(page.getByText(/On disk|На диске/).first()).toBeVisible()
  await expect(page.getByText(/Not downloaded|Нет на диске/)).toHaveCount(0)
})
