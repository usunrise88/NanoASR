import { expect, test } from '@playwright/test'

/**
 * What the SPA does when the server wants an API key.
 *
 * There is no configuration flag telling it: the first 401 is the answer, and
 * this is the test that the answer arrives as a usable prompt rather than as
 * three empty panels.
 *
 * Needs a server in apikey mode, which the default fixture is not, so it points
 * at its own: NANOASR_E2E_AUTH_URL and NANOASR_E2E_KEY.
 */
const url = process.env.NANOASR_E2E_AUTH_URL
const key = process.env.NANOASR_E2E_KEY

test.skip(!url || !key, 'set NANOASR_E2E_AUTH_URL and NANOASR_E2E_KEY')

test('asks for a key instead of showing empty screens', async ({ page }) => {
  await page.goto(`${url}/ui/`)

  // The sheet opens itself off the back of the first refused request.
  await expect(page.getByText(/wants a key|требует ключ/i)).toBeVisible()

  await page.getByPlaceholder('sk-…').fill(key ?? '')
  await page.getByRole('button', { name: /^(Save|Сохранить)$/ }).click()

  // With the key accepted, the model list is populated — which is only
  // possible if the retry after saving actually happened.
  await expect(page.locator('select').first().locator('option').first()).toHaveText(/\w/)
  await expect(page.getByText(/wants a key|требует ключ/i)).toHaveCount(0)
})

test('a wrong key leaves the prompt reachable from settings', async ({ page }) => {
  await page.goto(`${url}/ui/settings`)

  await page.getByPlaceholder('sk-…').fill('sk-definitely-not-the-key')
  await page.getByRole('button', { name: /^(Save|Сохранить)$/ }).click()

  await page.goto(`${url}/ui/models`)
  // Refused again, and the page says so rather than rendering an empty list as
  // though the server had no models.
  await expect(page.getByText(/went wrong|пошло не так|unauthorized|ключ/i).first()).toBeVisible()
})
