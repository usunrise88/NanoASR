import { expect, test, type Page } from '@playwright/test'

/**
 * The language key, in a browser: a multilingual model offers a list of
 * locales instead of a free field, the choice reaches the server, and the
 * result says what the run was decoded as.
 *
 * Needs the multilingual model on the server (`nanoasr models pull
 * nemotron-3.5-streaming-multi`); without it the suite skips, because a
 * checkout with only the small dev models must not fail.
 */
const MODEL = 'nemotron-3.5-streaming-multi'
const CLIP = '../testdata/audio/ru-16k.wav'

test.beforeEach(async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(String(e)))
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text())
  })
  await page.goto('/ui/')
  ;(page as Page & { __errors?: string[] }).__errors = errors
})

test.afterEach(async ({ page }) => {
  const errors = (page as Page & { __errors?: string[] }).__errors ?? []
  expect(errors, `console errors: ${errors.join(' | ')}`).toHaveLength(0)
})

async function chooseModel(page: Page) {
  const models = await page.request.get('/api/v1/models')
  const have = ((await models.json()) as { data: { id: string }[] }).data.some((m) => m.id === MODEL)
  test.skip(!have, `${MODEL} is not on this server`)

  // A native <select>: its options are picked, not clicked.
  await page.getByLabel(/^(Model|Модель)$/).selectOption(MODEL)
  await page.getByText(/^(Options|Параметры)$/).click()
}

test('a multilingual model offers its locales and says what it was told', async ({ page }) => {
  await chooseModel(page)

  // The model says what it is, in words rather than as "multi".
  await expect(page.getByText(/(Multilingual: 32 locales|Многоязычная: 32 локал)/)).toBeVisible()

  const language = page.getByLabel(/^(Language|Язык)$/)
  // Auto first, then the model's own list — thirty-two, each named and
  // carrying its code.
  await expect(language.locator('option')).toHaveCount(33)
  await expect(language.locator('option').first()).toHaveText(
    /(Detect automatically|Определить автоматически)/,
  )
  await expect(language.locator('option[value="pt-BR"]')).toHaveText(/— pt-BR$/)
  await expect(language.locator('option[value="ru-RU"]')).toHaveText(/— ru-RU$/)
  await language.selectOption('ru-RU')

  await page.setInputFiles('input[type=file]', CLIP)
  await page.getByRole('button', { name: /^(Transcribe|Распознать)$/ }).click()
  await page.waitForURL(/\/ui\/result\//)
  await expect(page.locator('[data-word="0"]')).toBeVisible()

  // The result reports the locale it was decoded as, not the word the user typed
  // or the model's "multi".
  await expect(page.getByText('ru-RU', { exact: true })).toBeVisible()
})

test('automatic is reported as the model having chosen', async ({ page }) => {
  await chooseModel(page)

  await page.setInputFiles('input[type=file]', CLIP)
  await page.getByRole('button', { name: /^(Transcribe|Распознать)$/ }).click()
  await page.waitForURL(/\/ui\/result\//)
  await expect(page.locator('[data-word="0"]')).toBeVisible()

  // "auto" in a column of facts would read like a result; the runtime does not
  // say which language it settled on, so the page says who decided.
  await expect(page.getByText(/(chosen by the model|выбран моделью)/)).toBeVisible()
})
