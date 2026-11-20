<template>
  <main class="login-shell">
    <section class="login-card">
      <div class="brand">Reach</div>
      <h1>Sign in</h1>
      <p class="muted">Manage your reverse SSH tunnels.</p>
      <div v-if="sessionMessage" :class="route.query.reason === 'signed-out-everywhere' ? 'login-info' : 'login-error'" role="alert">{{ sessionMessage }}</div>
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
        <p class="muted small">Off: you are signed out when this tab closes. Turn it on only on your own phone or computer.</p>
        <button class="btn btn-primary" :disabled="loading">
          {{ loading ? 'Signing in...' : 'Sign in' }}
        </button>
        <div v-if="error" class="login-error" role="alert">{{ error }}</div>
      </form>
    </section>
  </main>
</template>

<script setup lang="ts">
import { safeReturnPath } from '~/utils/auth'

const api = useReachApi()
const route = useRoute()
const sessionMessage = computed(() => {
  if (route.query.then === 'revoke-all') return 'Sign out everywhere did NOT happen: this login was no longer valid. Sign in, then confirm it again.'
  if (route.query.reason === 'signed-out-everywhere') return 'Signed out everywhere. Every dashboard login, including this one, has been revoked.'
  return route.query.reason === 'session-expired' ? 'Session expired, please sign in again.' : ''
})
// Deliberately empty: pre-filling a username leaks it and is wrong for most installs.
const username = ref('')
const password = ref('')
// Default off (daybreak P3-7): the token lives in sessionStorage unless the user opts in.
const remember = ref(false)
const loading = ref(false)
const error = ref('')

async function submit() {
  loading.value = true
  error.value = ''
  try {
    await api.login(username.value.trim(), password.value, remember.value)
    password.value = ''
    const next = route.query.then === 'revoke-all' ? '/?tab=settings&then=revoke-all' : safeReturnPath(route.query.next)
    await navigateTo(next, { replace: true })
  } catch (e: any) {
    error.value = e.message || 'Login failed'
  } finally {
    loading.value = false
  }
}
</script>
