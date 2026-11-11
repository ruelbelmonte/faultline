<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  api,
  ApiError,
  download,
  isTerminal,
  post,
  streamEvents,
  type Finding,
  type Point,
  type RunDocument,
  type Timeseries,
} from '../api'
import { duration, evidence, percent, time, verdictMark } from '../format'
import Timeline from './Timeline.vue'

const props = defineProps<{ id: string }>()

const doc = ref<RunDocument | null>(null)
const series = ref<Timeseries | null>(null)
const error = ref('')
const notice = ref('')
const busy = ref(false)
const abort = new AbortController()
let stopped = false

const active = computed(() => doc.value !== null && !isTerminal(doc.value.status))
const triggers = computed(() => doc.value?.faults.reduce((n, f) => n + f.triggers, 0) ?? 0)
const errorLimit = computed(() => doc.value?.hypothesis.find((h) => h.name === 'error_rate_max')?.threshold ?? null)
const peak = computed(() => {
  let max = 0
  for (const p of series.value?.series ?? []) {
    if (p.driver.requests > 0) max = Math.max(max, p.driver.errors / p.driver.requests)
  }
  return max
})
const routes = computed(() => Object.entries(doc.value?.metrics.proxy ?? {}).sort(([a], [b]) => a.localeCompare(b)))

async function load() {
  const [d, ts] = await Promise.all([
    api<RunDocument>(`/api/v1/runs/${props.id}`),
    api<Timeseries>(`/api/v1/runs/${props.id}/timeseries`),
  ])
  doc.value = d
  series.value = ts
}

function addPoint(p: Point) {
  const s = series.value
  if (!s) return
  const i = s.series.findIndex((q) => q.t === p.t)
  if (i >= 0) s.series[i] = p
  else s.series.push(p)
}

// Follows the run until it ends: live buckets come from the event stream,
// everything else is refetched whenever the status changes.
async function follow() {
  while (!stopped && active.value) {
    try {
      await streamEvents(`/api/v1/runs/${props.id}/events`, abort.signal, (e) => {
        if (e.event === 'metrics') addPoint(e.data as Point)
        else if (e.event === 'status') load().catch(() => {})
        else if (e.event === 'finding' && doc.value) {
          const f = e.data as Finding
          if (!doc.value.findings.some((x) => x.id === f.id && x.title === f.title)) doc.value.findings.push(f)
        }
      })
    } catch {
      if (stopped) return
    }
    if (stopped) return
    await load().catch(() => {})
    if (active.value) await new Promise((r) => setTimeout(r, 1000))
  }
}

async function abortRun() {
  busy.value = true
  notice.value = ''
  try {
    await post(`/api/v1/runs/${props.id}/abort`)
    notice.value = 'Run aborted.'
  } catch (e) {
    notice.value = e instanceof ApiError && e.status === 409 ? 'The run had already finished.' : 'The run could not be aborted.'
  } finally {
    busy.value = false
    load().catch(() => {})
  }
}

async function exportReport(format: string, ext: string) {
  notice.value = ''
  try {
    await download(`/api/v1/runs/${props.id}/report?format=${format}`, `${props.id}-report.${ext}`)
  } catch (e) {
    notice.value = e instanceof ApiError && e.status === 409 ? 'Reports are available once the run has finished.' : 'The report could not be downloaded.'
  }
}

onMounted(async () => {
  try {
    await load()
    follow()
  } catch (e) {
    error.value = e instanceof ApiError && e.status === 404 ? 'This run does not exist.' : 'The run could not be loaded.'
  }
})
onBeforeUnmount(() => {
  stopped = true
  abort.abort()
})
</script>

<template>
  <p><a href="#/runs">&larr; All runs</a></p>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <template v-if="doc">
    <h1>
      {{ doc.experiment.name }} <code class="run-id">{{ doc.run_id }}</code>
    </h1>
    <p v-if="doc.experiment.description" class="muted">{{ doc.experiment.description }}</p>

    <p class="verdict" :class="`v-${doc.verdict ?? 'pending'}`" role="status" data-testid="run-verdict">
      <span class="mark" aria-hidden="true">{{ verdictMark(doc.verdict) }}</span>
      <span>Verdict: {{ doc.verdict ?? 'in progress' }}</span>
      <span class="reason">status: {{ doc.status }}<template v-if="doc.reason">, reason: {{ doc.reason }}</template></span>
    </p>

    <div class="toolbar">
      <button v-if="active" class="danger" type="button" data-testid="abort-run" :disabled="busy" @click="abortRun">Abort run</button>
      <span class="group" role="group" aria-label="Export report">
        <button type="button" data-testid="export-html" :disabled="active" @click="exportReport('html', 'html')">Export HTML</button>
        <button type="button" data-testid="export-md" :disabled="active" @click="exportReport('md', 'md')">Export Markdown</button>
        <button type="button" data-testid="export-json" :disabled="active" @click="exportReport('json', 'json')">Export JSON</button>
        <button type="button" data-testid="export-junit" :disabled="active" @click="exportReport('junit', 'xml')">Export JUnit</button>
      </span>
    </div>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>

    <section class="tiles" aria-label="Key figures">
      <div class="tile">
        <div class="label">Requests</div>
        <div class="value" data-testid="kpi-requests">{{ doc.metrics.driver.requests }}</div>
        <div class="note">{{ doc.metrics.driver.errors }} errors during injection</div>
      </div>
      <div class="tile">
        <div class="label">Faults injected</div>
        <div class="value" data-testid="kpi-faults">{{ triggers }}</div>
        <div class="note">{{ doc.faults.length }} fault(s) configured</div>
      </div>
      <div class="tile">
        <div class="label">Peak error rate</div>
        <div class="value">{{ percent(peak) }}</div>
        <div class="note">{{ errorLimit === null ? 'no threshold evaluated yet' : `threshold ${percent(errorLimit)}` }}</div>
      </div>
      <div class="tile">
        <div class="label">Recovery time</div>
        <div class="value">{{ doc.recovery_seconds === null ? '–' : `${doc.recovery_seconds} s` }}</div>
        <div class="note">{{ doc.recovery_seconds === null ? 'not measured' : 'after faults cleared' }}</div>
      </div>
    </section>

    <section>
      <h2>Timeline</h2>
      <Timeline v-if="series" :series="series" :error-limit="errorLimit" />
      <details v-if="series">
        <summary>Timeline data</summary>
        <div class="scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col" class="n">Requests</th>
                <th scope="col" class="n">Errors</th>
                <th scope="col" class="n">p95 ms</th>
                <th scope="col" class="n">Faulted</th>
                <th scope="col" class="n">Probes ok</th>
                <th scope="col" class="n">Probes failed</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in series.series" :key="p.t">
                <td>{{ time(p.t) }}</td>
                <td class="n">{{ p.driver.requests }}</td>
                <td class="n">{{ p.driver.errors }}</td>
                <td class="n">{{ p.driver.p95_ms }}</td>
                <td class="n">{{ p.proxy.faulted }}</td>
                <td class="n">{{ p.probes.ok }}</td>
                <td class="n">{{ p.probes.fail }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </details>
    </section>

    <section>
      <h2>Hypothesis</h2>
      <p v-if="doc.hypothesis.length === 0" class="muted">
        {{ active ? 'Evaluated when the run ends.' : 'No hypothesis thresholds were evaluated.' }}
      </p>
      <div v-else class="scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Threshold</th>
              <th scope="col" class="n">Limit</th>
              <th scope="col" class="n">Observed</th>
              <th scope="col">Result</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="h in doc.hypothesis" :key="h.name">
              <td><code>{{ h.name }}</code></td>
              <td class="n">{{ h.threshold }}</td>
              <td class="n">{{ h.observed ?? 'no data' }}</td>
              <td :class="h.passed ? 'pass' : 'fail'">{{ h.passed ? '✓ pass' : '✕ FAIL' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section>
      <h2>Findings</h2>
      <p v-if="doc.findings.length === 0" class="muted">{{ active ? 'Evaluated when the run ends.' : 'No findings.' }}</p>
      <article v-for="(f, i) in doc.findings" :key="i" class="finding" :class="`s-${f.severity}`">
        <h3><span class="sev">{{ f.severity }}</span>{{ f.title }}</h3>
        <p>{{ f.detail }}</p>
        <table>
          <thead>
            <tr>
              <th scope="col">Evidence (<code>{{ f.id }}</code>)</th>
              <th scope="col">Value</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(v, k) in f.evidence" :key="k">
              <td><code>{{ k }}</code></td>
              <td>{{ evidence(v) }}</td>
            </tr>
          </tbody>
        </table>
      </article>
    </section>

    <section>
      <h2>Faults</h2>
      <div class="scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Fault</th>
              <th scope="col">Type</th>
              <th scope="col">Route</th>
              <th scope="col" class="n">Triggers</th>
              <th scope="col">Window</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="f in doc.faults" :key="f.id">
              <td><code>{{ f.id }}</code></td>
              <td>{{ f.type }}</td>
              <td>{{ f.route }}</td>
              <td class="n">{{ f.triggers }}</td>
              <td>{{ f.start_at && f.end_at ? `${time(f.start_at)} to ${time(f.end_at)}` : 'not installed' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section>
      <h2>Details</h2>
      <dl class="meta">
        <dt>Environment</dt>
        <dd>{{ doc.environment }}</dd>
        <dt>Started</dt>
        <dd>{{ time(doc.started_at) }}</dd>
        <dt>Duration</dt>
        <dd>{{ duration(doc.started_at, doc.ended_at) }}</dd>
        <dt>Driver p50 / p95</dt>
        <dd>{{ doc.metrics.driver.p50_ms }} ms / {{ doc.metrics.driver.p95_ms }} ms</dd>
        <template v-for="[name, m] in routes" :key="name">
          <dt>Route {{ name }}</dt>
          <dd>{{ m.requests }} proxied, {{ m.faulted }} faulted</dd>
        </template>
        <dt>Redacted headers</dt>
        <dd>{{ doc.redaction.headers.join(', ') || 'none' }}</dd>
      </dl>
    </section>
  </template>
</template>
