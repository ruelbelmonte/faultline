<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, ApiError, post, put, type ExperimentEntry, type ValidationError } from '../api'

const NAME = /^[a-z0-9-]+$/
const VALIDATE_DELAY_MS = 400

const list = ref<ExperimentEntry[]>([])
const name = ref('')
const yaml = ref('')
const errors = ref<ValidationError[]>([])
const validated = ref<'idle' | 'checking' | 'valid' | 'invalid'>('idle')
const message = ref('')
const failed = ref(false)
let timer: number | undefined
let sequence = 0

async function loadList() {
  try {
    list.value = await api<ExperimentEntry[]>('/api/v1/experiments')
  } catch {
    say('The experiments could not be loaded.', true)
  }
}

function say(text: string, isError = false) {
  message.value = text
  failed.value = isError
}

async function open(entry: ExperimentEntry) {
  try {
    const e = await api<{ name: string; yaml: string }>(`/api/v1/experiments/${entry.name}`)
    name.value = e.name
    yaml.value = e.yaml
    say('')
  } catch {
    say(`${entry.file} could not be opened.`, true)
  }
}

function blank() {
  name.value = ''
  yaml.value = ''
  errors.value = []
  validated.value = 'idle'
  say('')
}

async function validate() {
  const mine = ++sequence
  validated.value = 'checking'
  try {
    const res = await post<{ valid: boolean; errors: ValidationError[] }>('/api/v1/experiments/validate', { yaml: yaml.value })
    // A slower answer to older text must not overwrite a newer one.
    if (mine !== sequence) return
    errors.value = res.errors
    validated.value = res.valid ? 'valid' : 'invalid'
  } catch {
    if (mine === sequence) validated.value = 'idle'
  }
}

watch(yaml, (text) => {
  window.clearTimeout(timer)
  if (!text.trim()) {
    errors.value = []
    validated.value = 'idle'
    return
  }
  timer = window.setTimeout(validate, VALIDATE_DELAY_MS)
})

function showErrors(e: unknown, fallback: string) {
  if (e instanceof ApiError && e.errors.length > 0) {
    errors.value = e.errors
    validated.value = 'invalid'
    say('The experiment is not valid.', true)
  } else if (e instanceof ApiError && e.status === 409) {
    say('Another run is active. Wait for it to finish or use the kill switch.', true)
  } else {
    say(fallback, true)
  }
}

async function save() {
  if (!NAME.test(name.value)) {
    say('The name may only contain lowercase letters, digits and hyphens.', true)
    return
  }
  try {
    const res = await put<{ file: string }>(`/api/v1/experiments/${name.value}`, { yaml: yaml.value })
    say(`Saved ${res.file}.`)
    loadList()
  } catch (e) {
    showErrors(e, 'The experiment could not be saved.')
  }
}

async function run() {
  try {
    const res = await post<{ run_id: string }>('/api/v1/runs', { experiment_yaml: yaml.value })
    window.location.hash = `#/runs/${res.run_id}`
  } catch (e) {
    showErrors(e, 'The run could not be started.')
  }
}

onMounted(loadList)
onBeforeUnmount(() => window.clearTimeout(timer))
</script>

<template>
  <h1>Experiments</h1>
  <div class="split">
    <section aria-labelledby="files-title">
      <h2 id="files-title">Files</h2>
      <ul class="files">
        <li v-for="e in list" :key="e.file">
          <button type="button" class="link" @click="open(e)">{{ e.file }}</button>
          <span :class="e.valid ? 'pass' : 'fail'">{{ e.valid ? '✓ valid' : '✕ invalid' }}</span>
        </li>
        <li v-if="list.length === 0" class="muted">No experiment files yet.</li>
      </ul>
      <button type="button" @click="blank">New experiment</button>
    </section>

    <section aria-labelledby="editor-title">
      <h2 id="editor-title">Editor</h2>
      <label for="exp-name">Name (saved as <code>{name}.yaml</code>)</label>
      <input id="exp-name" v-model="name" type="text" spellcheck="false" autocomplete="off" data-testid="experiment-name" />
      <label for="exp-yaml">Experiment YAML</label>
      <textarea
        id="exp-yaml"
        v-model="yaml"
        rows="24"
        spellcheck="false"
        data-testid="experiment-yaml"
        :aria-invalid="validated === 'invalid' ? 'true' : undefined"
        aria-describedby="exp-status exp-errors"
      ></textarea>
      <p id="exp-status" role="status" data-testid="experiment-status" :class="{ pass: validated === 'valid', fail: validated === 'invalid', muted: validated === 'idle' || validated === 'checking' }">
        <template v-if="validated === 'valid'">✓ Valid</template>
        <template v-else-if="validated === 'invalid'">✕ {{ errors.length }} problem(s)</template>
        <template v-else-if="validated === 'checking'">Checking…</template>
        <template v-else>Validation runs as you type.</template>
      </p>
      <ul id="exp-errors" class="errors" data-testid="experiment-errors">
        <li v-for="(e, i) in errors" :key="i"><code>{{ e.path || '(document)' }}</code>: {{ e.message }}</li>
      </ul>
      <div class="toolbar">
        <button type="button" data-testid="experiment-save" :disabled="validated !== 'valid'" @click="save">Save</button>
        <button type="button" class="primary" data-testid="experiment-run" :disabled="validated !== 'valid'" @click="run">Run</button>
      </div>
      <p v-if="message" :class="failed ? 'error' : 'notice'" role="status">{{ message }}</p>
      <p class="muted">
        Values of sensitive headers (authorization, cookies, API keys) are never shown or saved here; they appear as
        <code>[REDACTED]</code>. An experiment that needs one has to be run from its file with <code>faultline run</code>.
      </p>
    </section>
  </div>
</template>
