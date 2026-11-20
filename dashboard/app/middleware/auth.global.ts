import { isJWTExpired, loginLocation } from '~/utils/auth'

export default defineNuxtRouteMiddleware((to) => {
  if (!import.meta.client || to.path === '/login') return

  const api = useReachApi()
  if (!api.token.value) return navigateTo(loginLocation(to.fullPath))
  if (isJWTExpired(api.token.value)) {
    api.setToken(null)
    return navigateTo(loginLocation(to.fullPath, 'session-expired'))
  }
})
