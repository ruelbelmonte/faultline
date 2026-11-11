<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api, isTerminal, type RunDocument, type RunSummary } from '../api'
import { duration, time, verdictMark } from '../format'

const runs = ref<RunSummary[]>([])
const loaded = ref(false)
const error = ref('')
// The list endpoint has no end time, so the duration comes from each run's document.
const ended = reactive<Record<string, string | null>>({})
const MAX_DETAIL = 50
let timer: number | undefined

async function load() {
  try {
    runs.value = await api<RunSummary[]>('/api/v1/runs')
    error.value = ''
    for (const r of runs.value.slice(0, MAX_DETAIL)) {
      if (!isTerminal(r.status) || ended[r.run_id] !== undefined) continue
      api<RunDocument>(`/api/v1/runs/${r.run_id}`)
        .then((doc) => (ended[r.run_id] = doc.ended_at))
        .catch(() => {})
    }
  } catch {
    error.value = 'The runs could not be loaded.'
  } finally {
    loaded.value = true
  }
}

function runDuration(r: RunSummary): string {
  if (!isTerminal(r.status)) return 'running'
  const end = ended[r.run_id]
  return end ? duration(r.started_at, end) : ''
}

onMounted(() => {
  load()
  timer = window.setInterval(load, 2000)
})
onBeforeUnmount(() => window.clearInterval(timer))
</script>

<template>
  <h1>Runs</h1>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <div class="scroll">
    <table data-testid="runs-table">
      <caption class="sr-only">
        Experiment runs, newest first
      </caption>
      <thead>
        <tr>
          <th scope="col">Run</th>
          <th scope="col">Experiment</th>
          <th scope="col">Status</th>
          <th scope="col">Verdict</th>
          <th scope="col">Started</th>
          <th scope="col">Duration</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in runs" :key="r.run_id">
          <td>
            <a :href="`#/runs/${r.run_id}`"><code>{{ r.run_id }}</code></a>
          </td>
          <td>{{ r.experiment }}</td>
          <td>{{ r.status }}</td>
          <td>
            <span v-if="r.verdict" class="verdict-chip" :class="`v-${r.verdict}`">
              <span aria-hidden="true">{{ verdictMark(r.verdict) }}</span> {{ r.verdict }}
            </span>
            <span v-else class="muted">in progress</span>
          </td>
          <td>{{ time(r.started_at) }}</td>
          <td>{{ runDuration(r) }}</td>
        </tr>
        <tr v-if="loaded && runs.length === 0">
          <td colspan="6" class="muted">
            No runs yet. Start one from <a href="#/experiments">Experiments</a> or with <code>faultline run</code>.
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
