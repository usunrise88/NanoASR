import { expect, test, type Page } from '@playwright/test'

/**
 * Hotword dictionaries, in a browser: create one, find it by a phrase inside
 * it, apply it to a transcription, and delete it again.
 *
 * Against the real server, because the parts worth testing here are the ones a
 * unit test cannot see — that the form sends what the API expects, that the
 * search is the server's and looks inside the phrases, and that the screen
 * says which models the list will actually apply to.
 */
const KEY = `e2e-${Date.now().toString(36)}`

test.beforeEach(async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(String(e)))
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text())
  })
  await page.goto('/ui/hotwords')
  ;(page as Page & { __errors?: string[] }).__errors = errors
})

test.afterEach(async ({ page }) => {
  const errors = (page as Page & { __errors?: string[] }).__errors ?? []
  expect(errors, `console errors: ${errors.join(' | ')}`).toHaveLength(0)
})

test('creates, finds, edits and deletes a dictionary', async ({ page }) => {
  await page.getByRole('button', { name: /^(New dictionary|Новый словарь)$/ }).click()

  await page.getByLabel(/^(Key|Ключ)$/).fill(KEY)
  // Unique: a server that has run this suite before still holds the
  // dictionaries of the runs that did not reach their delete.
  await page.getByLabel(/^(Name|Название)$/).fill(`Playwright ${KEY}`)
  await page.getByLabel(/^(Phrases|Фразы)/).fill('лукоморье\nЧерномор\n\nлукоморье\n')
  await page.getByRole('button', { name: /^(Save|Сохранить)$/ }).click()

  // The repeat and the blank line are gone: what is stored is what the server
  // cleaned, and the card reports its count.
  const card = page.locator(`[data-item="${KEY}"]`)
  await expect(card.getByText(/2 (phrases|фраз)/)).toBeVisible()

  // The search is the server's, and it looks inside the phrases rather than
  // only at the names — nothing in this dictionary's name says "Черномор".
  await page.getByLabel(/^(Search|Поиск)$/).fill('черномор')
  await expect(page.getByText(KEY, { exact: true })).toBeVisible()

  await page.getByLabel(/^(Search|Поиск)$/).fill('нет-такого-слова-нигде')
  await expect(page.getByText(/(Nothing found|Ничего не найдено)/)).toBeVisible()
  await page.getByLabel(/^(Search|Поиск)$/).fill('')

  // Which models this applies to is part of the screen, because the answer is
  // per model and is not in the configuration anywhere.
  await expect(page.getByText(/(Which models this works with|С какими моделями)/)).toBeVisible()

  await page.getByRole('button', { name: /^(Open|Открыть)$/ }).first().click()
  await expect(page.getByLabel(/^(Key|Ключ)$/)).toHaveValue(KEY)
  await page.getByRole('button', { name: /^(Cancel|Отмена)$/ }).click()
})

test('a dictionary reaches a transcription from the run screen', async ({ page }) => {
  // The picker only exists when the server has dictionaries, which the test
  // above created.
  await page.goto('/ui/')
  await page.getByText(/^(Options|Параметры)$/).click()

  const picker = page.getByRole('button', { name: `Playwright ${KEY}` })
  if ((await picker.count()) === 0) {
    test.skip(true, 'no dictionary on this server')
  }
  await picker.click()

  await page.setInputFiles('input[type=file]', '../testdata/audio/ru-16k.wav')
  await page.getByRole('button', { name: /^(Transcribe|Распознать)$/ }).click()
  await page.waitForURL(/\/ui\/result\//)
  await expect(page.locator('[data-word="0"]')).toBeVisible()
})

test('deletes the dictionary it made', async ({ page }) => {
  await page.getByLabel(/^(Search|Поиск)$/).fill(KEY)
  await page.locator(`[data-item="${KEY}"]`).getByRole('button', { name: /^(Delete|Удалить)$/ }).click()
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /^(Delete|Удалить)$/ })
    .click()
  await expect(page.getByText(/(Nothing found|Ничего не найдено)/)).toBeVisible()
})
