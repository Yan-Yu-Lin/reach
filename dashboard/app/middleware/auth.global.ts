import { isJWTExpired } from '~/utils/auth'

export default defineNuxtRouteMiddleware((to) => {
  if (!import.meta.client || to.path === '/login') return

  const api = useReachApi()
  if (!api.token.value) return navigateTo('/login')
  if (isJWTExpired(api.token.value)) {
    api.setToken(null)
    return navigateTo({ path: '/login', query: { reason: 'session-expired' } })
  }
})
