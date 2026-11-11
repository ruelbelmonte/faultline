<script setup lang="ts">
import { ref } from 'vue'
import { setToken, tokenWorks } from '../api'

const token = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  const value = token.value.trim()
  if (!value) {
    error.value = 'Enter the API token.'
    return
  }
  busy.value = true
  error.value = ''
  try {
    if (await tokenWorks(value)) setToken(value)
    else error.value = 'The control plane rejected this token.'
  } catch {
    error.value = 'The control plane could not be reached.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="gate">
    <form @submit.prevent="submit">
      <h1>Faultline</h1>
      <p>Enter the API token to continue. It is kept for this browser tab only.</p>
      <label for="token">API token (FAULTLINE_TOKEN)</label>
      <input
        id="token"
        v-model="token"
        type="password"
        autocomplete="off"
        spellcheck="false"
        data-testid="token-input"
        :aria-invalid="error ? 'true' : undefined"
        aria-describedby="token-error"
      />
      <p id="token-error" class="error" role="alert">{{ error }}</p>
      <button class="primary" type="submit" data-testid="token-submit" :disabled="busy">Continue</button>
    </form>
  </main>
</template>
