<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type AuditEntry } from '../api'
import { evidence, time } from '../format'

const entries = ref<AuditEntry[]>([])
const loaded = ref(false)
const error = ref('')

async function load() {
  try {
    entries.value = await api<AuditEntry[]>('/api/v1/audit?limit=200')
    error.value = ''
  } catch {
    error.value = 'The audit log could not be loaded.'
  } finally {
    loaded.value = true
  }
}

onMounted(load)
</script>

<template>
  <h1>Audit log</h1>
  <p class="muted">The newest 200 entries. The log is append-only.</p>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <p><button type="button" @click="load">Refresh</button></p>
  <div class="scroll">
    <table data-testid="audit-table">
      <thead>
        <tr>
          <th scope="col">Time</th>
          <th scope="col">Actor</th>
          <th scope="col">Action</th>
          <th scope="col">Run</th>
          <th scope="col">Detail</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="(e, i) in entries" :key="i">
          <td>{{ time(e.ts) }}</td>
          <td>{{ e.actor }}</td>
          <td><code>{{ e.action }}</code></td>
          <td>
            <a v-if="e.run_id" :href="`#/runs/${e.run_id}`"><code>{{ e.run_id }}</code></a>
          </td>
          <td>{{ evidence(e.detail) }}</td>
        </tr>
        <tr v-if="loaded && entries.length === 0">
          <td colspan="5" class="muted">Nothing has been recorded yet.</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
