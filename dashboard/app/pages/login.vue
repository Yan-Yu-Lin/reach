<template>
  <main class="login-shell">
    <section class="login-card">
      <div class="brand">Reach</div>
      <h1>Sign in</h1>
      <p class="muted">Manage your reverse SSH tunnels.</p>
      <div v-if="sessionMessage" class="login-error">{{ sessionMessage }}</div>
      <form @submit.prevent="submit">
        <label>
          Username
          <input v-model="username" autocomplete="username" autofocus />
        </label>
        <label>
          Password
          <input v-model="password" type="password" autocomplete="current-password" />
        </label>
        <button class="btn btn-primary" :disabled="loading">
          {{ loading ? 'Signing in...' : 'Sign in' }}
        </button>
        <div v-if="error" class="login-error">{{ error }}</div>
      </form>
    </section>
  </main>
</template>

<script setup lang="ts">
const api = useReachApi()
const route = useRoute()
const sessionMessage = computed(() => route.query.reason === 'session-expired' ? 'Session expired, please sign in again.' : '')
const username = ref('admin')
const password = ref('')
const loading = ref(false)
const error = ref('')

async function submit() {
  loading.value = true
  error.value = ''
  try {
    await api.login(username.value, password.value)
    await navigateTo('/')
  } catch (e: any) {
    error.value = e.message || 'Login failed'
  } finally {
    loading.value = false
  }
}
</script>
