<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, isTerminal, post, session, type Meta, type RunSummary } from './api'
import TokenGate from './components/TokenGate.vue'
import RunsList from './components/RunsList.vue'
import RunDetail from './components/RunDetail.vue'
import Experiments from './components/Experiments.vue'
import AuditLog from './components/AuditLog.vue'

const hash = ref(window.location.hash || '#/runs')
const onHash = () => (hash.value = window.location.hash || '#/runs')

const route = computed(() => {
  const parts = hash.value.replace(/^#\/?/, '').split('/')
  if (parts[0] === 'runs' && parts[1]) return { page: 'run', id: parts[1] }
  if (parts[0] === 'experiments') return { page: 'experiments', id: '' }
  if (parts[0] === 'audit') return { page: 'audit', id: '' }
  return { page: 'runs', id: '' }
})

const meta = ref<Meta | null>(null)
const activeRun = ref<RunSummary | null>(null)
const killDialog = ref<HTMLDialogElement | null>(null)
const killMessage = ref('')
const killBusy = ref(false)
let poll: number | undefined

async function refresh() {
  if (!session.token) return
  try {
    if (!meta.value) meta.value = await api<Meta>('/api/v1/meta')
    const runs = await api<RunSummary[]>('/api/v1/runs')
    activeRun.value = runs.find((r) => !isTerminal(r.status)) ?? null
  } catch {
    // A 401 clears the token and brings the gate back; anything else shows on the next poll.
  }
}

async function kill() {
  killBusy.value = true
  killMessage.value = ''
  try {
    await post('/api/v1/kill')
    killMessage.value = 'All faults cleared.'
  } catch {
    killMessage.value = 'The kill switch could not reach the proxy. Faults still expire with their lease.'
  } finally {
    killBusy.value = false
    killDialog.value?.close()
    refresh()
  }
}

watch(
  () => session.token,
  (token) => {
    if (token) refresh()
    else meta.value = null
  },
)

onMounted(() => {
  window.addEventListener('hashchange', onHash)
  refresh()
  poll = window.setInterval(refresh, 2000)
})
onBeforeUnmount(() => {
  window.removeEventListener('hashchange', onHash)
  window.clearInterval(poll)
})
</script>

<template>
  <TokenGate v-if="!session.token" />
  <template v-else>
    <a class="skip" href="#main">Skip to content</a>
    <header class="top">
      <a class="brand" href="#/runs">Faultline</a>
      <nav aria-label="Main">
        <a href="#/runs" :aria-current="route.page === 'runs' || route.page === 'run' ? 'page' : undefined">Runs</a>
        <a href="#/experiments" :aria-current="route.page === 'experiments' ? 'page' : undefined">Experiments</a>
        <a href="#/audit" :aria-current="route.page === 'audit' ? 'page' : undefined">Audit log</a>
      </nav>
      <div class="top-right">
        <span class="badge" title="Environment from faultline.yaml">env: {{ meta?.environment ?? '…' }}</span>
        <a v-if="activeRun" class="badge active" :href="`#/runs/${activeRun.run_id}`" data-testid="active-run">
          <span aria-hidden="true">●</span> active run: {{ activeRun.status }}
        </a>
        <span v-else class="badge" data-testid="active-run">no active run</span>
        <button class="danger" type="button" data-testid="kill-switch" @click="killDialog?.showModal()">Kill switch</button>
      </div>
    </header>

    <p v-if="killMessage" class="notice" role="status" data-testid="kill-result">{{ killMessage }}</p>

    <dialog ref="killDialog" aria-labelledby="kill-title">
      <h2 id="kill-title">Use the kill switch?</h2>
      <p>This clears every fault on the proxy and aborts the active run. It cannot be undone.</p>
      <div class="actions">
        <button type="button" @click="killDialog?.close()">Cancel</button>
        <button class="danger" type="button" data-testid="kill-confirm" :disabled="killBusy" @click="kill">Clear all faults</button>
      </div>
    </dialog>

    <main id="main" tabindex="-1">
      <RunDetail v-if="route.page === 'run'" :key="route.id" :id="route.id" />
      <Experiments v-else-if="route.page === 'experiments'" />
      <AuditLog v-else-if="route.page === 'audit'" />
      <RunsList v-else />
    </main>
  </template>
</template>
