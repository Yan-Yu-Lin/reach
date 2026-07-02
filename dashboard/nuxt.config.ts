export default defineNuxtConfig({
  compatibilityDate: '2026-06-29',
  ssr: false,
  devtools: { enabled: false },
  css: ['~/assets/css/main.css'],
  app: {
    head: {
      title: 'Reach Dashboard',
      meta: [{ name: 'viewport', content: 'width=device-width, initial-scale=1' }]
    }
  },
  runtimeConfig: {
    public: {
      apiBase: process.env.NUXT_PUBLIC_API_BASE || '/api',
      reachBaseUrl: process.env.NUXT_PUBLIC_REACH_BASE_URL || ''
    }
  }
})
