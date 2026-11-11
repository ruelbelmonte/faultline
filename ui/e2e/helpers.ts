import { expect, type APIRequestContext, type Page } from '@playwright/test'

export const TOKEN = 'ui-smoke-control-token-0123'
export const auth = { Authorization: `Bearer ${TOKEN}` }

export function experiment(name: string, duration: string): string {
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

export async function createRun(request: APIRequestContext, yaml: string): Promise<string> {
  const resp = await request.post('/api/v1/runs', { headers: auth, data: { experiment_yaml: yaml } })
  expect(resp.status(), await resp.text()).toBe(201)
  return (await resp.json()).run_id
}

export async function waitForNoActiveRun(request: APIRequestContext) {
  await expect
    .poll(async () => {
      const runs = await (await request.get('/api/v1/runs', { headers: auth })).json()
      return runs.every((r: { status: string }) => r.status === 'completed' || r.status === 'aborted')
    }, { timeout: 60_000 })
    .toBe(true)
}

/** Opens the app already signed in, the way a returning tab would be. */
export async function signedIn(page: Page, hash = '#/runs') {
  await page.addInitScript((token) => sessionStorage.setItem('faultline.token', token), TOKEN)
  await page.goto('/' + hash)
}
