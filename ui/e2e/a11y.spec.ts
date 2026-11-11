import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { auth, createRun, experiment, signedIn, waitForNoActiveRun } from './helpers'

// Every page of the dashboard, in light and dark, checked with axe against
// WCAG 2.1 A and AA plus axe's best-practice rules.

test.describe.configure({ mode: 'serial' })

async function expectNoViolations(page: Page, what: string) {
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme })
    const results = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'best-practice'])
      .analyze()
    const found = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.help}\n` + v.nodes.map((n) => `    ${n.target.join(' ')}  ${n.failureSummary?.replace(/\s+/g, ' ')}`).join('\n'),
    )
    expect(found, `${what}, ${colorScheme} mode`).toEqual([])
  }
}

let finishedRun = ''

test.beforeAll(async ({ request }) => {
  await waitForNoActiveRun(request)
  finishedRun = await createRun(request, experiment('ui-a11y', '2s'))
  await waitForNoActiveRun(request)
})

test('token gate', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByTestId('token-input')).toBeVisible()
  await expectNoViolations(page, 'token gate')

  await page.getByTestId('token-input').fill('not-the-token-0123456789')
  await page.getByTestId('token-submit').click()
  await expect(page.getByRole('alert')).toBeVisible()
  await expectNoViolations(page, 'token gate with a rejected token')
})

test('runs list', async ({ page }) => {
  await signedIn(page)
  await expect(page.getByTestId('runs-table').getByRole('row').filter({ hasText: finishedRun })).toBeVisible()
  await expectNoViolations(page, 'runs list')
})

test('run detail of a finished run', async ({ page }) => {
  await signedIn(page, `#/runs/${finishedRun}`)
  await expect(page.getByTestId('run-verdict')).toContainText('failed')
  await expect(page.getByText('steady_state_breach').first()).toBeVisible()
  await expect(page.locator('.timeline canvas')).toBeVisible()
  await expectNoViolations(page, 'run detail, finished')
})

test('run detail of an active run, and the kill switch dialog', async ({ page, request }) => {
  const id = await createRun(request, experiment('ui-a11y-active', '60s'))
  try {
    await signedIn(page, `#/runs/${id}`)
    await expect(page.getByTestId('run-verdict')).toContainText('injecting', { timeout: 30_000 })
    await expect(page.getByTestId('abort-run')).toBeVisible()
    await expectNoViolations(page, 'run detail, active')

    await page.getByTestId('kill-switch').click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await expectNoViolations(page, 'kill switch dialog')
    await page.getByRole('button', { name: 'Cancel' }).click()
  } finally {
    await request.post(`/api/v1/runs/${id}/abort`, { headers: auth })
    await waitForNoActiveRun(request)
  }
})

test('experiments editor with a valid experiment', async ({ page }) => {
  await signedIn(page, '#/experiments')
  await page.getByTestId('experiment-yaml').fill(experiment('ui-a11y-editor', '2s'))
  await expect(page.getByTestId('experiment-status')).toContainText('Valid')
  await expectNoViolations(page, 'experiments, valid')
})

test('experiments editor with validation errors', async ({ page }) => {
  await signedIn(page, '#/experiments')
  await page.getByTestId('experiment-yaml').fill(experiment('ui-a11y-editor', '2s').replace('type: http_error', 'type: gremlin'))
  // The status line, not the error list: once axe has run in a Firefox
  // process, polling the text of the error list crashes the page (DECISIONS D86).
  await expect(page.getByTestId('experiment-status')).toContainText('1 problem')
  await expectNoViolations(page, 'experiments, with errors')
  expect(await page.getByTestId('experiment-errors').locator('li').count()).toBe(1)
})

test('audit log', async ({ page }) => {
  await signedIn(page, '#/audit')
  await expect(page.getByTestId('audit-table').getByText('run_created', { exact: true }).first()).toBeVisible()
  await expectNoViolations(page, 'audit log')
})
