import { reactive } from 'vue'

const TOKEN_KEY = 'faultline.token'

function readToken(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export const session = reactive({ token: readToken() })

export function setToken(token: string) {
  session.token = token
  try {
    sessionStorage.setItem(TOKEN_KEY, token)
  } catch {
    // Storage can be unavailable; the token then lives for this page only.
  }
}

export function clearToken() {
  session.token = ''
  try {
    sessionStorage.removeItem(TOKEN_KEY)
  } catch {
    // Nothing to remove.
  }
}

export interface ValidationError {
  path: string
  message: string
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    public errors: ValidationError[] = [],
  ) {
    super(code ? `${status} ${code}` : `HTTP ${status}`)
  }
}

async function send(path: string, token: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers)
  headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')
  const resp = await fetch(path, { ...init, headers })
  if (resp.status === 401) {
    // A rejected token is forgotten at once; the gate asks again.
    if (token === session.token) clearToken()
    throw new ApiError(401, 'unauthorized')
  }
  if (!resp.ok) {
    let code = ''
    let errors: ValidationError[] = []
    try {
      const body = await resp.json()
      code = body.error ?? ''
      errors = body.errors ?? []
    } catch {
      // Not JSON; the status code says enough.
    }
    throw new ApiError(resp.status, code, errors)
  }
  return resp
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const resp = await send(path, session.token, init)
  return (await resp.json()) as T
}

export function post<T>(path: string, body?: unknown): Promise<T> {
  return api<T>(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) })
}

export function put<T>(path: string, body: unknown): Promise<T> {
  return api<T>(path, { method: 'PUT', body: JSON.stringify(body) })
}

/** Checks a token against the API without storing it. */
export async function tokenWorks(token: string): Promise<boolean> {
  try {
    await send('/api/v1/meta', token)
    return true
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return false
    throw e
  }
}

/** Fetches a file with the token and hands it to the browser as a download. */
export async function download(path: string, filename: string) {
  const resp = await send(path, session.token)
  const url = URL.createObjectURL(await resp.blob())
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

export interface ServerEvent {
  event: string
  data: unknown
}

/**
 * Reads a Server-Sent Events stream with fetch, because EventSource cannot
 * send the Authorization header. Resolves when the server ends the stream.
 */
export async function streamEvents(path: string, signal: AbortSignal, onEvent: (e: ServerEvent) => void) {
  const resp = await send(path, session.token, { signal })
  const reader = resp.body!.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) return
    buffer += decoder.decode(value, { stream: true })
    let end: number
    while ((end = buffer.indexOf('\n\n')) >= 0) {
      const block = buffer.slice(0, end)
      buffer = buffer.slice(end + 2)
      let event = 'message'
      let data = ''
      for (const line of block.split('\n')) {
        if (line.startsWith('event: ')) event = line.slice(7)
        else if (line.startsWith('data: ')) data += line.slice(6)
      }
      if (!data) continue
      try {
        onEvent({ event, data: JSON.parse(data) })
      } catch {
        // A malformed event is skipped; the next refetch corrects the view.
      }
    }
  }
}

export interface Meta {
  environment: string
  allow_unscoped: boolean
  scope_header: string
}

export interface RunSummary {
  run_id: string
  experiment: string
  status: string
  verdict: string | null
  started_at: string
}

export interface HypothesisEntry {
  name: string
  threshold: number
  observed: number | null
  passed: boolean
}

export interface FaultResult {
  id: string
  type: string
  route: string
  triggers: number
  start_at: string | null
  end_at: string | null
}

export interface Finding {
  id: string
  severity: string
  title: string
  detail: string
  evidence: Record<string, unknown>
}

export interface RunDocument {
  schema_version: string
  run_id: string
  experiment: { name: string; description: string; yaml_sha256: string }
  environment: string
  status: string
  verdict: string | null
  reason: string | null
  started_at: string
  ended_at: string | null
  hypothesis: HypothesisEntry[]
  faults: FaultResult[]
  findings: Finding[]
  metrics: {
    driver: { requests: number; errors: number; error_rate: number; p50_ms: number; p95_ms: number }
    proxy: Record<string, { requests: number; faulted: number }>
  }
  recovery_seconds: number | null
  redaction: { enabled: boolean; headers: string[] }
}

export interface Point {
  t: string
  driver: { requests: number; errors: number; p95_ms: number }
  proxy: { requests: number; faulted: number }
  probes: { ok: number; fail: number }
}

export interface Timeseries {
  bucket_seconds: number
  series: Point[]
  phases: { name: string; start: string; end: string | null }[]
  fault_windows: { fault_id: string; start: string; end: string }[]
  markers: { type: string; t: string; detail: string }[]
}

export interface ExperimentEntry {
  name: string
  file: string
  valid: boolean
}

export interface AuditEntry {
  ts: string
  actor: string
  action: string
  run_id: string
  detail: Record<string, unknown>
}

export const isTerminal = (status: string) => status === 'completed' || status === 'aborted'
