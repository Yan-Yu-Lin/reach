import { isJWTExpired, loginLocation } from '~/utils/auth'

export default defineNuxtRouteMiddleware((to) => {
  if (!import.meta.client || to.path === '/login') return

  const api = useReachApi()
  if (!api.token.value) return navigateTo(loginLocation(to.fullPath))
  if (isJWTExpired(api.token.value)) {
    // Only this tab's own (expired) token; another tab's remembered login stays (R3-01).
    api.signOutLocally(api.token.value, 'expired')
    return navigateTo(loginLocation(to.fullPath, 'session-expired'))
  }
})
