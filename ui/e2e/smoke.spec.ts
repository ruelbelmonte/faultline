import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { readFile } from 'node:fs/promises'

const TOKEN = 'ui-smoke-control-token-0123'
const auth = { Authorization: `Bearer ${TOKEN}` }

function experiment(name: string, duration: string): string {
  return `version: 1
name: ${name}
description: UI smoke test experiment
target: {route: inventory}
duration: ${duration}
traffic:
  base_url: http://127.0.0.1:19000
  rps: 10
  concurrency: 4
  requests:
    - {method: GET, path: /api/orders/1}
steady_state:
  precheck: {consecutive_ok: 1, timeout: 5s}
  probes:
    - {name: shop, url: "http://127.0.0.1:19000/health", interval: 200ms}
  hypothesis:
    error_rate_max: 0.05
recovery: {timeout: 5s}
faults:
  - id: f1
    type: http_error
    match: {path: "/stock/*"}
    params: {status: 503}
`
}

async function createRun(request: APIRequestContext, yaml: string): Promise<string> {
  const resp = await request.post('/api/v1/runs', { headers: auth, data: { experiment_yaml: yaml } })
  expect(resp.status(), await resp.text()).toBe(201)
  return (await resp.json()).run_id
}

async function waitForNoActiveRun(request: APIRequestContext) {
  await expect
    .poll(async () => {
      const runs = await (await request.get('/api/v1/runs', { headers: auth })).json()
      return runs.every((r: { status: string }) => r.status === 'completed' || r.status === 'aborted')
    }, { timeout: 60_000 })
    .toBe(true)
}

/** Opens the app already signed in, the way a returning tab would be. */
async function signedIn(page: Page, hash = '#/runs') {
  await page.addInitScript((token) => sessionStorage.setItem('faultline.token', token), TOKEN)
  await page.goto('/' + hash)
}

test.describe.configure({ mode: 'serial' })

test('token gate rejects a wrong token and accepts the right one', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByTestId('token-input')).toBeVisible()
  await expect(page.getByTestId('runs-table')).toHaveCount(0)
  await expect(page.getByTestId('kill-switch')).toHaveCount(0)

  await page.getByTestId('token-input').fill('not-the-token-0123456789')
  await page.getByTestId('token-submit').click()
  await expect(page.getByRole('alert')).toContainText('rejected')
  await expect(page.getByTestId('runs-table')).toHaveCount(0)

  await page.getByTestId('token-input').fill(TOKEN)
  await page.getByTestId('token-submit').click()
  await expect(page.getByTestId('runs-table')).toBeVisible()
  await expect(page.getByTestId('kill-switch')).toBeVisible()
  await expect(page.getByText('env: ci')).toBeVisible()
  expect(await page.evaluate(() => sessionStorage.getItem('faultline.token'))).toBe(TOKEN)
  expect(await page.evaluate(() => localStorage.length)).toBe(0)

  await page.reload()
  await expect(page.getByTestId('runs-table')).toBeVisible()
})

test('a rejected token is cleared and the gate comes back', async ({ page }) => {
  await page.addInitScript(() => sessionStorage.setItem('faultline.token', 'stale-token-0123456789'))
  await page.goto('/')
  await expect(page.getByTestId('token-input')).toBeVisible()
  expect(await page.evaluate(() => sessionStorage.getItem('faultline.token'))).toBeNull()
})

test('runs list, run detail verdict and export buttons', async ({ page, request }) => {
  const id = await createRun(request, experiment('ui-failing', '3s'))
  await signedIn(page)

  const row = page.getByTestId('runs-table').getByRole('row').filter({ hasText: id })
  await expect(row).toBeVisible()
  await expect(row).toContainText('ui-failing')
  await expect(page.getByTestId('active-run')).toContainText('active run')

  await row.getByRole('link').click()
  const verdict = page.getByTestId('run-verdict')
  await expect(verdict).toBeVisible()
  await expect(verdict).toContainText('failed', { timeout: 60_000 })
  await expect(verdict).toContainText('completed')

  // The name appears in the hypothesis table and again as finding evidence.
  await expect(page.getByRole('cell', { name: 'error_rate_max' }).first()).toBeVisible()
  await expect(page.getByText('FAIL').first()).toBeVisible()
  await expect(page.getByText('steady_state_breach').first()).toBeVisible()
  await expect(page.getByText('retry_amplification').first()).toBeVisible()
  expect(Number(await page.getByTestId('kpi-requests').innerText())).toBeGreaterThan(10)
  expect(Number(await page.getByTestId('kpi-faults').innerText())).toBeGreaterThan(10)
  await expect(page.locator('.timeline canvas')).toBeVisible()
  await expect(page.getByTestId('abort-run')).toHaveCount(0)

  const exports: [string, string, RegExp][] = [
    ['export-html', `${id}-report.html`, /Verdict: failed[\s\S]*<svg/],
    ['export-md', `${id}-report.md`, /\*\*Verdict: FAILED\*\*/],
    ['export-json', `${id}-report.json`, new RegExp(`"run_id": "${id}"`)],
    ['export-junit', `${id}-report.xml`, /<testsuite name="ui-failing"/],
  ]
  for (const [testid, filename, content] of exports) {
    const button = page.getByTestId(testid)
    await expect(button).toBeEnabled()
    const [download] = await Promise.all([page.waitForEvent('download'), button.click()])
    expect(download.suggestedFilename()).toBe(filename)
    expect(await readFile(await download.path(), 'utf8')).toMatch(content)
  }

  await page.goto('/#/runs')
  await expect(page.getByTestId('runs-table').getByRole('row').filter({ hasText: id })).toContainText('failed')
  await expect(page.getByTestId('active-run')).toContainText('no active run')
})

test('kill switch asks for confirmation, clears faults and aborts the run', async ({ page, request }) => {
  await waitForNoActiveRun(request)
  const id = await createRun(request, experiment('ui-kill', '60s'))
  await signedIn(page, `#/runs/${id}`)

  const verdict = page.getByTestId('run-verdict')
  await expect(verdict).toContainText('injecting', { timeout: 30_000 })
  await expect(page.getByTestId('abort-run')).toBeVisible()
  await expect(page.getByTestId('export-json')).toBeDisabled()
  await expect
    .poll(async () => (await (await request.get('/api/v1/faults/active', { headers: auth })).json()).run_id)
    .toBe(id)

  // Cancelling the dialog does nothing.
  await page.getByTestId('kill-switch').click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByRole('dialog')).toBeHidden()
  expect((await (await request.get('/api/v1/faults/active', { headers: auth })).json()).run_id).toBe(id)

  await page.getByTestId('kill-switch').click()
  await page.getByTestId('kill-confirm').click()
  await expect(page.getByTestId('kill-result')).toContainText('cleared')
  await expect(verdict).toContainText('aborted', { timeout: 30_000 })
  await expect(verdict).toContainText('kill_switch')

  const active = await (await request.get('/api/v1/faults/active', { headers: auth })).json()
  expect(active.run_id).toBeNull()
  expect(active.faults).toEqual([])
  await expect(page.getByTestId('active-run')).toContainText('no active run')
})

test('experiment editor validates as you type, saves and runs', async ({ page, request }) => {
  await waitForNoActiveRun(request)
  await signedIn(page, '#/experiments')

  const editor = page.getByTestId('experiment-yaml')
  await editor.fill(experiment('ui-editor', '2s').replace('type: http_error', 'type: gremlin'))
  await expect(page.getByTestId('experiment-status')).toContainText('1 problem')
  await expect(page.getByTestId('experiment-errors')).toContainText('faults[0].type')
  await expect(page.getByTestId('experiment-run')).toBeDisabled()

  await editor.fill(experiment('ui-editor', '2s'))
  await expect(page.getByTestId('experiment-status')).toContainText('Valid')
  await expect(page.getByTestId('experiment-errors')).toBeEmpty()

  await page.getByTestId('experiment-name').fill('ui-editor')
  await page.getByTestId('experiment-save').click()
  await expect(page.getByRole('button', { name: 'ui-editor.yaml' })).toBeVisible()

  await page.getByTestId('experiment-run').click()
  await expect(page.getByTestId('run-verdict')).toContainText('failed', { timeout: 60_000 })
})

test('audit log shows what was done', async ({ page }) => {
  await signedIn(page, '#/audit')
  const table = page.getByTestId('audit-table')
  for (const action of ['run_created', 'kill_switch', 'run_aborted', 'report_downloaded', 'experiment_saved']) {
    await expect(table.getByText(action, { exact: true }).first()).toBeVisible()
  }
})

test('the page can be used with the keyboard alone', async ({ page }) => {
  await signedIn(page)
  await expect(page.getByTestId('runs-table')).toBeVisible()
  await page.keyboard.press('Tab')
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused()
  // Tab until the kill switch has focus, then open and dismiss its dialog with the keyboard.
  for (let i = 0; i < 12 && !(await page.getByTestId('kill-switch').evaluate((el) => el === document.activeElement)); i++) {
    await page.keyboard.press('Tab')
  }
  await expect(page.getByTestId('kill-switch')).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toBeHidden()
})
