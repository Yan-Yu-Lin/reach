<template>
  <main class="login-shell">
    <section class="login-card">
      <div class="brand">Reach</div>
      <h1>Sign in</h1>
      <p class="muted">Manage your reverse SSH tunnels.</p>
      <div v-if="sessionMessage" class="login-error" role="alert">{{ sessionMessage }}</div>
      <form @submit.prevent="submit">
        <label>
          Username
          <input
            v-model="username"
            class="input"
            name="username"
            autocomplete="username"
            autocapitalize="off"
            autocorrect="off"
            spellcheck="false"
            required
            autofocus
          />
        </label>
        <label>
          Password
          <input v-model="password" class="input" name="password" type="password" autocomplete="current-password" required />
        </label>
        <label class="login-remember">
          <input v-model="remember" type="checkbox" />
          <span>Keep me signed in on this device</span>
        </label>
        <p class="muted small">Turn this off on a shared computer: you are signed out when the tab closes.</p>
        <button class="btn btn-primary" :disabled="loading">
          {{ loading ? 'Signing in...' : 'Sign in' }}
        </button>
        <div v-if="error" class="login-error" role="alert">{{ error }}</div>
      </form>
    </section>
  </main>
</template>

<script setup lang="ts">
const api = useReachApi()
const route = useRoute()
const sessionMessage = computed(() => route.query.reason === 'session-expired' ? 'Session expired, please sign in again.' : '')
// Deliberately empty: pre-filling a username leaks it and is wrong for most installs.
const username = ref('')
const password = ref('')
const remember = ref(true)
const loading = ref(false)
const error = ref('')

async function submit() {
  loading.value = true
  error.value = ''
  try {
    await api.login(username.value.trim(), password.value, remember.value)
    password.value = ''
    await navigateTo('/')
  } catch (e: any) {
    error.value = e.message || 'Login failed'
  } finally {
    loading.value = false
  }
}
</script>
